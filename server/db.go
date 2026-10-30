package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	seen_at       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS movies_title ON movies(title COLLATE NOCASE);

CREATE TABLE IF NOT EXISTS progress (
	movie_id   INTEGER PRIMARY KEY REFERENCES movies(id) ON DELETE CASCADE,
	position_s INTEGER NOT NULL,
	duration_s INTEGER NOT NULL,
	watched    INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL
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
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &DB{db}, nil
}

const movieCols = `m.id, m.path, m.title, m.year, m.tmdb_id, m.overview, m.runtime_s,
	m.poster_path, m.backdrop_path, m.media_json, m.added_at,
	COALESCE(p.position_s, 0), COALESCE(p.watched, 0)`

func (d *DB) ListMovies(sort, q string) ([]Movie, error) {
	order := "m.title COLLATE NOCASE"
	switch sort {
	case "added":
		order = "m.added_at DESC"
	case "year":
		order = "m.year DESC, m.title COLLATE NOCASE"
	}
	rows, err := d.Query(`SELECT `+movieCols+`
		FROM movies m LEFT JOIN progress p ON p.movie_id = m.id
		WHERE ? = '' OR m.title LIKE '%' || ? || '%'
		ORDER BY `+order, q, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Movie
	for rows.Next() {
		m, err := scanMovie(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	if out == nil {
		out = []Movie{}
	}
	return out, rows.Err()
}

var ErrNotFound = errors.New("not found")

func (d *DB) GetMovie(id int64) (*Movie, error) {
	row := d.QueryRow(`SELECT `+movieCols+`
		FROM movies m LEFT JOIN progress p ON p.movie_id = m.id WHERE m.id = ?`, id)
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
	if withMedia && mediaJ.Valid && mediaJ.String != "" {
		var mi MediaInfo
		if err := json.Unmarshal([]byte(mediaJ.String), &mi); err == nil {
			m.Media = &mi
		}
	}
	return &m, nil
}

// fileRow is the scanner's view of an existing entry.
type fileRow struct {
	ID     int64
	Size   int64
	MTime  int64
	TMDBID int64
}

func (d *DB) LookupFile(path string) (*fileRow, error) {
	var r fileRow
	var tmdb sql.NullInt64
	err := d.QueryRow(`SELECT id, size, mtime, tmdb_id FROM movies WHERE path = ?`, path).
		Scan(&r.ID, &r.Size, &r.MTime, &tmdb)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.TMDBID = tmdb.Int64
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

// DeleteUnseen drops entries whose file was not seen by the scan that started at `since`.
func (d *DB) DeleteUnseen(since time.Time) (int64, error) {
	res, err := d.Exec(`DELETE FROM movies WHERE seen_at < ?`, since.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (d *DB) SetMetadata(id int64, md Metadata) error {
	_, err := d.Exec(`UPDATE movies SET tmdb_id = ?, title = ?, year = ?, overview = ?, runtime_s = ?,
		poster_path = ?, backdrop_path = ? WHERE id = ?`,
		nullInt64(md.TMDBID), md.Title, nullInt(md.Year), md.Overview, md.RuntimeS,
		md.PosterPath, md.BackdropPath, id)
	return err
}

func (d *DB) SetProgress(id int64, positionS, durationS int) error {
	watched := 0
	if durationS > 0 && float64(positionS) >= float64(durationS)*0.95 {
		watched = 1
	}
	_, err := d.Exec(`INSERT INTO progress(movie_id, position_s, duration_s, watched, updated_at)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(movie_id) DO UPDATE SET position_s = excluded.position_s,
			duration_s = excluded.duration_s, watched = excluded.watched, updated_at = excluded.updated_at`,
		id, positionS, durationS, watched, time.Now().UTC().Format(time.RFC3339))
	return err
}

func (d *DB) CountMovies() (int, error) {
	var n int
	err := d.QueryRow(`SELECT COUNT(*) FROM movies`).Scan(&n)
	return n, err
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
