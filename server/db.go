package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo needed on Windows
)

type DB struct {
	*sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS movies (
	id            INTEGER PRIMARY KEY,
	path          TEXT UNIQUE NOT NULL,
	title         TEXT NOT NULL,
	year          INTEGER,
	tmdb_id       INTEGER,
	overview      TEXT,
	runtime_s     INTEGER,
	poster_path   TEXT,
	backdrop_path TEXT,
	media_json    TEXT,
	size          INTEGER NOT NULL,
	mtime         INTEGER NOT NULL,
	added_at      TEXT NOT NULL,
	seen_at       TEXT NOT NULL,
	match_ver     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS movies_title ON movies(title COLLATE NOCASE);

CREATE TABLE IF NOT EXISTS shows (
	id            INTEGER PRIMARY KEY,
	key           TEXT UNIQUE NOT NULL,
	title         TEXT NOT NULL,
	year          INTEGER,
	tmdb_id       INTEGER,
	overview      TEXT,
	poster_path   TEXT,
	backdrop_path TEXT,
	added_at      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS episodes (
	id         INTEGER PRIMARY KEY,
	show_id    INTEGER NOT NULL REFERENCES shows(id) ON DELETE CASCADE,
	season     INTEGER NOT NULL,
	episode    INTEGER NOT NULL,
	title      TEXT NOT NULL,
	overview   TEXT,
	runtime_s  INTEGER,
	still_path TEXT,
	meta       INTEGER NOT NULL DEFAULT 0,
	path       TEXT UNIQUE NOT NULL,
	media_json TEXT,
	size       INTEGER NOT NULL,
	mtime      INTEGER NOT NULL,
	added_at   TEXT NOT NULL,
	seen_at    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS episodes_show ON episodes(show_id, season, episode);

-- kind is "movie" or "episode"; item_id the row in that table
CREATE TABLE IF NOT EXISTS progress (
	kind       TEXT NOT NULL,
	item_id    INTEGER NOT NULL,
	position_s INTEGER NOT NULL,
	duration_s INTEGER NOT NULL,
	watched    INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL,
	PRIMARY KEY (kind, item_id)
);

CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

func OpenDB(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // sqlite is happiest with one writer
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	db.Exec(`ALTER TABLE movies ADD COLUMN match_ver INTEGER NOT NULL DEFAULT 0`) // older DBs; fails harmlessly when present
	if _, err := db.Exec(`INSERT OR IGNORE INTO progress
		SELECT 'movie', movie_id, position_s, duration_s, watched, updated_at FROM progress_v1`); err == nil {
		db.Exec(`DROP TABLE progress_v1`)
	}
	return &DB{db}, nil
}

// migrate handles the one schema change so far: progress used to be keyed by
// movie_id only. Rename it so the new table can be created and back-filled.
func migrate(db *sql.DB) error {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('progress') WHERE name = 'movie_id'`).Scan(&n)
	if err != nil || n == 0 {
		return nil
	}
	_, err = db.Exec(`ALTER TABLE progress RENAME TO progress_v1`)
	return err
}

// ---------------------------------------------------------------- movies

const movieCols = `m.id, m.path, m.title, m.year, m.tmdb_id, m.overview, m.runtime_s,
	m.poster_path, m.backdrop_path, m.media_json, m.added_at,
	COALESCE(p.position_s, 0), COALESCE(p.watched, 0)`

const movieFrom = `FROM movies m LEFT JOIN progress p ON p.kind = 'movie' AND p.item_id = m.id`

func (d *DB) ListMovies(sort, q string) ([]Movie, error) {
	order := "m.title COLLATE NOCASE"
	switch sort {
	case "added":
		order = "m.added_at DESC"
	case "year":
		order = "m.year DESC, m.title COLLATE NOCASE"
	}
	rows, err := d.Query(`SELECT `+movieCols+` `+movieFrom+`
		WHERE ? = '' OR m.title LIKE '%' || ? || '%'
		ORDER BY `+order, q, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Movie{}
	for rows.Next() {
		m, err := scanMovie(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

var ErrNotFound = errors.New("not found")

func (d *DB) GetMovie(id int64) (*Movie, error) {
	row := d.QueryRow(`SELECT `+movieCols+` `+movieFrom+` WHERE m.id = ?`, id)
	m, err := scanMovie(row, true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

type rowScanner interface{ Scan(dest ...any) error }

func scanMovie(r rowScanner, withMedia bool) (*Movie, error) {
	var (
		m                                  Movie
		year, tmdbID, runtime              sql.NullInt64
		overview, poster, backdrop, mediaJ sql.NullString
		added                              string
		watched                            int
	)
	err := r.Scan(&m.ID, &m.Path, &m.Title, &year, &tmdbID, &overview, &runtime,
		&poster, &backdrop, &mediaJ, &added, &m.ResumeS, &watched)
	if err != nil {
		return nil, err
	}
	m.Year = int(year.Int64)
	m.TMDBID = tmdbID.Int64
	m.RuntimeS = int(runtime.Int64)
	m.Overview = overview.String
	m.posterPath = poster.String
	m.backdropPath = backdrop.String
	m.Watched = watched != 0
	m.AddedAt, _ = time.Parse(time.RFC3339, added)
	if m.posterPath != "" {
		m.Poster = fmt.Sprintf("/api/v1/movies/%d/poster", m.ID)
	}
	if m.backdropPath != "" {
		m.Backdrop = fmt.Sprintf("/api/v1/movies/%d/backdrop", m.ID)
	}
	if withMedia {
		m.Media = unmarshalMedia(mediaJ)
	}
	return &m, nil
}

// fileRow is the scanner's view of an existing entry (movie or episode).
type fileRow struct {
	ID       int64
	Size     int64
	MTime    int64
	HasMeta  bool // movie: tmdb_id set; episode: meta flag
	MatchVer int  // movie: matcher version of the last TMDB attempt

	ShowID          int64 // episode placement, to move it when parsing improves
	Season, Episode int
}

func (d *DB) LookupFile(path string) (*fileRow, error) {
	var r fileRow
	var tmdb sql.NullInt64
	err := d.QueryRow(`SELECT id, size, mtime, tmdb_id, match_ver FROM movies WHERE path = ?`, path).
		Scan(&r.ID, &r.Size, &r.MTime, &tmdb, &r.MatchVer)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.HasMeta = tmdb.Valid && tmdb.Int64 != 0
	return &r, nil
}

func (d *DB) InsertFile(path, title string, year int, size, mtime int64, media *MediaInfo, now time.Time) (int64, error) {
	mj, err := marshalMedia(media)
	if err != nil {
		return 0, err
	}
	ts := now.UTC().Format(time.RFC3339)
	res, err := d.Exec(`INSERT INTO movies(path, title, year, media_json, size, mtime, added_at, seen_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, path, title, nullInt(year), mj, size, mtime, ts, ts)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *DB) UpdateFile(id int64, size, mtime int64, media *MediaInfo, now time.Time) error {
	mj, err := marshalMedia(media)
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE movies SET size = ?, mtime = ?, media_json = ?, seen_at = ? WHERE id = ?`,
		size, mtime, mj, now.UTC().Format(time.RFC3339), id)
	return err
}

func (d *DB) TouchSeen(id int64, now time.Time) error {
	_, err := d.Exec(`UPDATE movies SET seen_at = ? WHERE id = ?`, now.UTC().Format(time.RFC3339), id)
	return err
}

// DeleteUnseen drops movies and episodes whose file was not seen by the scan
// that started at `since`, then shows left without episodes and orphaned
// progress. Files under `keep` (folders the scan could not read) stay.
func (d *DB) DeleteUnseen(since time.Time, keep []string) (int64, error) {
	ts := since.UTC().Format(time.RFC3339)
	where, args := `seen_at < ?`, []any{ts}
	for _, k := range keep {
		where += ` AND substr(path, 1, length(?)) != ?`
		args = append(args, k, k)
	}
	res, err := d.Exec(`DELETE FROM movies WHERE `+where, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	res, err = d.Exec(`DELETE FROM episodes WHERE `+where, args...)
	if err != nil {
		return n, err
	}
	ne, _ := res.RowsAffected()
	d.Exec(`DELETE FROM shows WHERE NOT EXISTS (SELECT 1 FROM episodes e WHERE e.show_id = shows.id)`)
	d.Exec(`DELETE FROM progress WHERE kind = 'movie' AND item_id NOT IN (SELECT id FROM movies)`)
	d.Exec(`DELETE FROM progress WHERE kind = 'episode' AND item_id NOT IN (SELECT id FROM episodes)`)
	return n + ne, nil
}

func (d *DB) SetTitle(id int64, title string, year int) {
	d.Exec(`UPDATE movies SET title = ?, year = ? WHERE id = ? AND tmdb_id IS NULL`, title, nullInt(year), id)
}

// MergeDuplicateShows folds shows matched to the same TMDB series (folders
// named differently, or split before shows were merged during scans) into
// the oldest of them. Returns how many were folded.
func (d *DB) MergeDuplicateShows() (int, error) {
	rows, err := d.Query(`SELECT tmdb_id, MIN(id) FROM shows WHERE tmdb_id IS NOT NULL
		GROUP BY tmdb_id HAVING COUNT(*) > 1`)
	if err != nil {
		return 0, err
	}
	type dup struct{ tmdb, keep int64 }
	var dups []dup
	for rows.Next() {
		var x dup
		rows.Scan(&x.tmdb, &x.keep)
		dups = append(dups, x)
	}
	rows.Close()
	n := 0
	for _, x := range dups {
		res, err := d.Exec(`UPDATE episodes SET show_id = ? WHERE show_id IN
			(SELECT id FROM shows WHERE tmdb_id = ? AND id != ?)`, x.keep, x.tmdb, x.keep)
		if err != nil {
			return n, err
		}
		res, err = d.Exec(`DELETE FROM shows WHERE tmdb_id = ? AND id != ?`, x.tmdb, x.keep)
		if err != nil {
			return n, err
		}
		c, _ := res.RowsAffected()
		n += int(c)
	}
	return n, nil
}

func (d *DB) SetMatchVersion(id int64, v int) {
	d.Exec(`UPDATE movies SET match_ver = ? WHERE id = ?`, v, id)
}

func (d *DB) SetMetadata(id int64, md Metadata) error {
	_, err := d.Exec(`UPDATE movies SET tmdb_id = ?, title = ?, year = ?, overview = ?, runtime_s = ?,
		poster_path = ?, backdrop_path = ? WHERE id = ?`,
		nullInt64(md.TMDBID), md.Title, nullInt(md.Year), md.Overview, md.RuntimeS,
		md.PosterPath, md.BackdropPath, id)
	return err
}

// UnmatchedItem is a movie or show without TMDB data and where it lives.
type UnmatchedItem struct {
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	Year     int    `json:"year,omitempty"`
	Path     string `json:"path"`               // the movie file, or one episode of the show
	Episodes int    `json:"episodes,omitempty"` // shows
}

func (d *DB) Unmatched(limit int) (movies, shows []UnmatchedItem, err error) {
	movies, shows = []UnmatchedItem{}, []UnmatchedItem{}
	rows, err := d.Query(`SELECT id, title, COALESCE(year, 0), path FROM movies WHERE tmdb_id IS NULL
		ORDER BY path LIMIT ?`, limit)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var u UnmatchedItem
		if err := rows.Scan(&u.ID, &u.Title, &u.Year, &u.Path); err != nil {
			rows.Close()
			return nil, nil, err
		}
		movies = append(movies, u)
	}
	rows.Close()
	rows, err = d.Query(`SELECT s.id, s.title, COALESCE(s.year, 0), MIN(e.path), COUNT(*) FROM shows s
		JOIN episodes e ON e.show_id = s.id WHERE s.tmdb_id IS NULL GROUP BY s.id ORDER BY s.title LIMIT ?`, limit)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var u UnmatchedItem
		if err := rows.Scan(&u.ID, &u.Title, &u.Year, &u.Path, &u.Episodes); err != nil {
			return nil, nil, err
		}
		shows = append(shows, u)
	}
	return movies, shows, rows.Err()
}

type EpisodeFile struct {
	ID      int64  `json:"id"`
	Season  int    `json:"season"`
	Episode int    `json:"episode"`
	Title   string `json:"title"`
	Path    string `json:"path"`
}

func (d *DB) ShowFiles(showID int64) ([]EpisodeFile, error) {
	rows, err := d.Query(`SELECT id, season, episode, title, path FROM episodes WHERE show_id = ?
		ORDER BY season, episode, title`, showID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EpisodeFile{}
	for rows.Next() {
		var f EpisodeFile
		if err := rows.Scan(&f.ID, &f.Season, &f.Episode, &f.Title, &f.Path); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (d *DB) CountMovies() (int, error) {
	var n int
	err := d.QueryRow(`SELECT COUNT(*) FROM movies`).Scan(&n)
	return n, err
}

// ---------------------------------------------------------------- shows

var showKeyRe = regexp.MustCompile(`[^a-z0-9]+`)

// ShowKey normalises a series title so "Bob's Burgers (2011)" and
// "bobs.burgers" land on the same show. The year is left out: folder names
// carry it inconsistently; shows that still split are merged by TMDB id.
func ShowKey(title string, year int) string {
	return showKeyRe.ReplaceAllString(strings.ToLower(title), "")
}

// ShowByTMDB finds another show already matched to the same TMDB series.
func (d *DB) ShowByTMDB(tmdbID, except int64) int64 {
	var id int64
	d.QueryRow(`SELECT id FROM shows WHERE tmdb_id = ? AND id != ? ORDER BY id LIMIT 1`, tmdbID, except).Scan(&id)
	return id
}

// MergeShow moves every episode of `from` into `into` and drops `from`.
func (d *DB) MergeShow(from, into int64) error {
	if _, err := d.Exec(`UPDATE episodes SET show_id = ? WHERE show_id = ?`, into, from); err != nil {
		return err
	}
	_, err := d.Exec(`DELETE FROM shows WHERE id = ?`, from)
	return err
}

// UpsertShow returns the show id for a key, creating the row if needed.
func (d *DB) UpsertShow(key, title string, year int, now time.Time) (id int64, created bool, err error) {
	err = d.QueryRow(`SELECT id FROM shows WHERE key = ?`, key).Scan(&id)
	if err == nil {
		return id, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	res, err := d.Exec(`INSERT INTO shows(key, title, year, added_at) VALUES(?, ?, ?, ?)`,
		key, title, nullInt(year), now.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, false, err
	}
	id, err = res.LastInsertId()
	return id, true, err
}

func (d *DB) ShowNeedsMetadata(id int64) bool {
	var tmdb sql.NullInt64
	if err := d.QueryRow(`SELECT tmdb_id FROM shows WHERE id = ?`, id).Scan(&tmdb); err != nil {
		return false
	}
	return !tmdb.Valid || tmdb.Int64 == 0
}

func (d *DB) ShowTMDBID(id int64) int64 {
	var tmdb sql.NullInt64
	d.QueryRow(`SELECT tmdb_id FROM shows WHERE id = ?`, id).Scan(&tmdb)
	return tmdb.Int64
}

func (d *DB) SetShowMetadata(id int64, md Metadata) error {
	_, err := d.Exec(`UPDATE shows SET tmdb_id = ?, title = ?, year = ?, overview = ?,
		poster_path = ?, backdrop_path = ? WHERE id = ?`,
		nullInt64(md.TMDBID), md.Title, nullInt(md.Year), md.Overview, md.PosterPath, md.BackdropPath, id)
	return err
}

const showCols = `s.id, s.title, s.year, s.tmdb_id, s.overview, s.poster_path, s.backdrop_path, s.added_at,
	(SELECT COUNT(DISTINCT e.season) FROM episodes e WHERE e.show_id = s.id),
	(SELECT COUNT(*) FROM episodes e WHERE e.show_id = s.id),
	(SELECT COUNT(*) FROM episodes e LEFT JOIN progress p ON p.kind = 'episode' AND p.item_id = e.id
		WHERE e.show_id = s.id AND COALESCE(p.watched, 0) = 0)`

func (d *DB) ListShows(q string) ([]Show, error) {
	rows, err := d.Query(`SELECT `+showCols+` FROM shows s
		WHERE ? = '' OR s.title LIKE '%' || ? || '%' ORDER BY s.title COLLATE NOCASE`, q, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Show{}
	for rows.Next() {
		s, err := scanShow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// GetShow returns the show with its episodes grouped by season and the next
// episode to watch (the first one not marked watched).
func (d *DB) GetShow(id int64) (*Show, error) {
	s, err := scanShow(d.QueryRow(`SELECT `+showCols+` FROM shows s WHERE s.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT `+episodeCols+` `+episodeFrom+` WHERE e.show_id = ? ORDER BY e.season, e.episode, e.title`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		ep, err := scanEpisode(rows, false)
		if err != nil {
			return nil, err
		}
		if n := len(s.SeasonList); n == 0 || s.SeasonList[n-1].Season != ep.Season {
			s.SeasonList = append(s.SeasonList, Season{Season: ep.Season})
		}
		last := &s.SeasonList[len(s.SeasonList)-1]
		last.Episodes = append(last.Episodes, *ep)
		if s.NextEpisode == nil && !ep.Watched {
			cp := *ep
			s.NextEpisode = &cp
		}
	}
	return s, rows.Err()
}

func scanShow(r rowScanner) (*Show, error) {
	var (
		s                          Show
		year, tmdbID               sql.NullInt64
		overview, poster, backdrop sql.NullString
		added                      string
	)
	err := r.Scan(&s.ID, &s.Title, &year, &tmdbID, &overview, &poster, &backdrop, &added,
		&s.Seasons, &s.Episodes, &s.Unwatched)
	if err != nil {
		return nil, err
	}
	s.Year = int(year.Int64)
	s.TMDBID = tmdbID.Int64
	s.Overview = overview.String
	s.posterPath = poster.String
	s.backdropPath = backdrop.String
	s.AddedAt, _ = time.Parse(time.RFC3339, added)
	if s.posterPath != "" {
		s.Poster = fmt.Sprintf("/api/v1/shows/%d/poster", s.ID)
	}
	if s.backdropPath != "" {
		s.Backdrop = fmt.Sprintf("/api/v1/shows/%d/backdrop", s.ID)
	}
	return &s, nil
}

func (d *DB) CountShows() (shows, episodes int) {
	d.QueryRow(`SELECT COUNT(*) FROM shows`).Scan(&shows)
	d.QueryRow(`SELECT COUNT(*) FROM episodes`).Scan(&episodes)
	return
}

// ---------------------------------------------------------------- episodes

const episodeCols = `e.id, e.show_id, s.title, e.season, e.episode, e.title, e.overview, e.runtime_s,
	e.still_path, e.added_at, e.path, e.media_json, COALESCE(p.position_s, 0), COALESCE(p.watched, 0)`

const episodeFrom = `FROM episodes e JOIN shows s ON s.id = e.show_id
	LEFT JOIN progress p ON p.kind = 'episode' AND p.item_id = e.id`

func (d *DB) GetEpisode(id int64) (*Episode, error) {
	ep, err := scanEpisode(d.QueryRow(`SELECT `+episodeCols+` `+episodeFrom+` WHERE e.id = ?`, id), true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return ep, err
}

func scanEpisode(r rowScanner, withMedia bool) (*Episode, error) {
	var (
		ep                      Episode
		overview, still, mediaJ sql.NullString
		runtime                 sql.NullInt64
		added                   string
		watched                 int
	)
	err := r.Scan(&ep.ID, &ep.ShowID, &ep.ShowTitle, &ep.Season, &ep.Episode, &ep.Title, &overview, &runtime,
		&still, &added, &ep.Path, &mediaJ, &ep.ResumeS, &watched)
	if err != nil {
		return nil, err
	}
	ep.Overview = overview.String
	ep.RuntimeS = int(runtime.Int64)
	ep.stillPath = still.String
	ep.Watched = watched != 0
	ep.AddedAt, _ = time.Parse(time.RFC3339, added)
	if ep.stillPath != "" {
		ep.Still = fmt.Sprintf("/api/v1/episodes/%d/still", ep.ID)
	}
	if withMedia {
		ep.Media = unmarshalMedia(mediaJ)
	}
	return &ep, nil
}

func (d *DB) LookupEpisodeFile(path string) (*fileRow, error) {
	var r fileRow
	var meta int
	err := d.QueryRow(`SELECT id, size, mtime, meta, show_id, season, episode FROM episodes WHERE path = ?`, path).
		Scan(&r.ID, &r.Size, &r.MTime, &meta, &r.ShowID, &r.Season, &r.Episode)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.HasMeta = meta != 0
	return &r, nil
}

func (d *DB) InsertEpisode(showID int64, season, episode int, title, path string, size, mtime int64, media *MediaInfo, now time.Time) (int64, error) {
	mj, err := marshalMedia(media)
	if err != nil {
		return 0, err
	}
	ts := now.UTC().Format(time.RFC3339)
	res, err := d.Exec(`INSERT INTO episodes(show_id, season, episode, title, path, media_json, size, mtime, added_at, seen_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, showID, season, episode, title, path, mj, size, mtime, ts, ts)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *DB) UpdateEpisodeFile(id int64, size, mtime int64, media *MediaInfo, now time.Time) error {
	mj, err := marshalMedia(media)
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE episodes SET size = ?, mtime = ?, media_json = ?, seen_at = ? WHERE id = ?`,
		size, mtime, mj, now.UTC().Format(time.RFC3339), id)
	return err
}

// PlaceEpisode moves an episode to another show / number; its TMDB data is
// fetched again for the new place.
func (d *DB) PlaceEpisode(id, showID int64, season, episode int, title string) error {
	_, err := d.Exec(`UPDATE episodes SET show_id = ?, season = ?, episode = ?, title = ?, overview = NULL,
		runtime_s = NULL, still_path = NULL, meta = 0 WHERE id = ?`, showID, season, episode, title, id)
	return err
}

func (d *DB) TouchEpisodeSeen(id int64, now time.Time) error {
	_, err := d.Exec(`UPDATE episodes SET seen_at = ? WHERE id = ?`, now.UTC().Format(time.RFC3339), id)
	return err
}

// EpisodesNeedingMeta lists (id, season, episode) for a show's episodes that
// have no TMDB metadata yet.
func (d *DB) EpisodesNeedingMeta(showID int64) ([][3]int64, error) {
	rows, err := d.Query(`SELECT id, season, episode FROM episodes WHERE show_id = ? AND meta = 0 ORDER BY season, episode`, showID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][3]int64
	for rows.Next() {
		var r [3]int64
		if err := rows.Scan(&r[0], &r[1], &r[2]); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) SetEpisodeMetadata(id int64, md EpisodeMeta) error {
	_, err := d.Exec(`UPDATE episodes SET title = ?, overview = ?, runtime_s = ?, still_path = ?, meta = 1 WHERE id = ?`,
		md.Title, md.Overview, md.RuntimeS, md.StillPath, id)
	return err
}

// ---------------------------------------------------------------- shared

// SetProgress records a playback position for a movie or an episode.
// Positions within the last 5 % mark the item watched; the first 30 s don't
// count as a resume point (someone just peeked).
func (d *DB) SetProgress(kind string, id int64, positionS, durationS int) error {
	if positionS < 30 {
		positionS = 0
	}
	watched := 0
	if durationS > 0 && float64(positionS) >= float64(durationS)*0.95 {
		watched = 1
	}
	_, err := d.Exec(`INSERT INTO progress(kind, item_id, position_s, duration_s, watched, updated_at)
		VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(kind, item_id) DO UPDATE SET position_s = excluded.position_s,
			duration_s = excluded.duration_s, watched = excluded.watched, updated_at = excluded.updated_at`,
		kind, id, positionS, durationS, watched, time.Now().UTC().Format(time.RFC3339))
	return err
}

func (d *DB) GetSetting(key string) (string, error) {
	var v string
	err := d.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (d *DB) SetSetting(key, value string) error {
	_, err := d.Exec(`INSERT INTO settings(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func marshalMedia(m *MediaInfo) (any, error) {
	if m == nil {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func unmarshalMedia(s sql.NullString) *MediaInfo {
	if !s.Valid || s.String == "" {
		return nil
	}
	var mi MediaInfo
	if err := json.Unmarshal([]byte(s.String), &mi); err != nil {
		return nil
	}
	return &mi
}

func nullInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
