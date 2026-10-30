package server

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var videoExt = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".mov": true, ".avi": true,
	".ts": true, ".m2ts": true, ".wmv": true, ".webm": true, ".mpg": true, ".mpeg": true,
}

// Scanner walks the library folders, probes new or changed files and keeps
// the movies / shows / episodes tables in sync. One scan at a time.
type Scanner struct {
	cfg  Config
	db   *DB
	tmdb *TMDB // nil when no API key is configured
	log  *slog.Logger

	mu     sync.Mutex
	status ScanStatus

	// per-scan caches
	shows   map[string]int64 // show key -> id
	touched map[int64]bool   // shows seen this scan (episode metadata pass)
}

func NewScanner(cfg Config, db *DB, tmdb *TMDB, log *slog.Logger) *Scanner {
	return &Scanner{cfg: cfg, db: db, tmdb: tmdb, log: log}
}

func (s *Scanner) Status() ScanStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.status
	if n, err := s.db.CountMovies(); err == nil {
		st.Movies = n
	}
	st.Shows, st.Episodes = s.db.CountShows()
	return st
}

// Start kicks off a scan in the background. Returns false if one is already running.
func (s *Scanner) Start(ctx context.Context, folders []string) bool {
	s.mu.Lock()
	if s.status.Scanning {
		s.mu.Unlock()
		return false
	}
	s.status.Scanning = true
	s.status.FilesSeen = 0
	s.status.LastError = ""
	s.mu.Unlock()

	go func() {
		err := s.scan(ctx, folders)
		now := time.Now()
		s.mu.Lock()
		s.status.Scanning = false
		s.status.LastScan = &now
		if err != nil {
			s.status.LastError = err.Error()
		}
		s.mu.Unlock()
	}()
	return true
}

func (s *Scanner) scan(ctx context.Context, folders []string) error {
	start := time.Now()
	s.shows = map[string]int64{}
	s.touched = map[int64]bool{}
	s.log.Info("scan started", "folders", folders)
	for _, root := range folders {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				s.log.Warn("scan: skipping", "path", path, "err", err)
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if d.IsDir() {
				if strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !videoExt[strings.ToLower(filepath.Ext(d.Name()))] || looksLikeExtra(d.Name()) {
				return nil
			}
			if err := s.file(ctx, root, path, d); err != nil {
				s.log.Warn("scan: file", "path", path, "err", err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	s.episodeMetadata(ctx)
	removed, err := s.db.DeleteUnseen(start)
	if err != nil {
		return err
	}
	s.log.Info("scan finished", "removed", removed, "took", time.Since(start).Round(time.Millisecond))
	return nil
}

func (s *Scanner) file(ctx context.Context, root, path string, d fs.DirEntry) error {
	info, err := d.Info()
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.status.FilesSeen++
	s.mu.Unlock()

	base := strings.TrimSuffix(d.Name(), filepath.Ext(d.Name()))
	if ep, ok := parseEpisode(base); ok {
		return s.episodeFile(ctx, root, path, info.Size(), info.ModTime().Unix(), ep)
	}
	return s.movieFile(ctx, root, path, info.Size(), info.ModTime().Unix())
}

func (s *Scanner) movieFile(ctx context.Context, root, path string, size, mtime int64) error {
	now := time.Now()
	existing, err := s.db.LookupFile(path)
	if err != nil {
		return err
	}
	if existing != nil && existing.Size == size && existing.MTime == mtime {
		if !existing.HasMeta && s.tmdb != nil {
			s.enrichMovie(ctx, existing.ID, path, root)
		}
		return s.db.TouchSeen(existing.ID, now)
	}

	media, err := Probe(ctx, s.cfg.FFprobe, path)
	if err != nil {
		return err
	}
	if existing != nil {
		return s.db.UpdateFile(existing.ID, size, mtime, media, now)
	}

	title, year := guessTitle(path, root)
	id, err := s.db.InsertFile(path, title, year, size, mtime, media, now)
	if err != nil {
		return err
	}
	s.log.Info("scan: added", "title", title, "year", year)
	if s.tmdb != nil {
		s.enrichMovie(ctx, id, path, root)
	}
	return nil
}

func (s *Scanner) enrichMovie(ctx context.Context, id int64, path, root string) {
	title, year := guessTitle(path, root)
	md, err := s.tmdb.Match(ctx, title, year)
	if err != nil {
		s.log.Warn("tmdb: no match", "title", title, "year", year, "err", err)
		return
	}
	if err := s.db.SetMetadata(id, md); err != nil {
		s.log.Warn("tmdb: save", "id", id, "err", err)
	}
}

func (s *Scanner) episodeFile(ctx context.Context, root, path string, size, mtime int64, ep episodeName) error {
	now := time.Now()
	series, year := ep.Series, ep.Year
	if series == "" {
		series, year = seriesFromDirs(path, root)
	}
	if series == "" {
		series = "Unknown show"
	}

	key := ShowKey(series, year)
	showID, ok := s.shows[key]
	if !ok {
		id, created, err := s.db.UpsertShow(key, series, year, now)
		if err != nil {
			return err
		}
		showID = id
		s.shows[key] = id
		if created {
			s.log.Info("scan: new show", "title", series, "year", year)
		}
		if s.tmdb != nil && (created || s.db.ShowNeedsMetadata(id)) {
			if md, err := s.tmdb.MatchShow(ctx, series, year); err != nil {
				s.log.Warn("tmdb: no show match", "title", series, "year", year, "err", err)
			} else if err := s.db.SetShowMetadata(id, md); err != nil {
				s.log.Warn("tmdb: save show", "id", id, "err", err)
			}
		}
	}
	s.touched[showID] = true

	existing, err := s.db.LookupEpisodeFile(path)
	if err != nil {
		return err
	}
	if existing != nil && existing.Size == size && existing.MTime == mtime {
		return s.db.TouchEpisodeSeen(existing.ID, now)
	}
	media, err := Probe(ctx, s.cfg.FFprobe, path)
	if err != nil {
		return err
	}
	if existing != nil {
		return s.db.UpdateEpisodeFile(existing.ID, size, mtime, media, now)
	}
	title := ep.Title
	if title == "" {
		title = fmt.Sprintf("Episode %d", ep.Episode)
	}
	if _, err := s.db.InsertEpisode(showID, ep.Season, ep.Episode, title, path, size, mtime, media, now); err != nil {
		return err
	}
	s.log.Info("scan: added episode", "show", series, "season", ep.Season, "episode", ep.Episode)
	return nil
}

// episodeMetadata fills in titles, overviews and stills for episodes that
// have none, one TMDB request per show season.
func (s *Scanner) episodeMetadata(ctx context.Context) {
	if s.tmdb == nil {
		return
	}
	for showID := range s.touched {
		tmdbID := s.db.ShowTMDBID(showID)
		if tmdbID == 0 {
			continue
		}
		need, err := s.db.EpisodesNeedingMeta(showID)
		if err != nil || len(need) == 0 {
			continue
		}
		seasons := map[int64]map[int]EpisodeMeta{}
		for _, r := range need {
			id, season, episode := r[0], r[1], int(r[2])
			eps, ok := seasons[season]
			if !ok {
				eps, err = s.tmdb.Season(ctx, tmdbID, int(season))
				if err != nil {
					s.log.Warn("tmdb: season", "show", tmdbID, "season", season, "err", err)
					eps = nil
				}
				seasons[season] = eps
			}
			if md, ok := eps[episode]; ok {
				if err := s.db.SetEpisodeMetadata(id, md); err != nil {
					s.log.Warn("tmdb: save episode", "id", id, "err", err)
				}
			}
		}
	}
}

// --- filename parsing ---

var (
	yearRe  = regexp.MustCompile(`^(.+?)[\s._\-(\[]+((?:19|20)\d{2})(?:[\s._\-)\]]|$)`)
	noiseRe = regexp.MustCompile(`(?i)[\s._\-](1080p|2160p|720p|480p|4k|uhd|bluray|blu-ray|bdrip|brrip|web-?dl|webrip|hdtv|dvdrip|x264|x265|h\.?264|h\.?265|hevc|remux|hdr|dv|atmos|dts|aac|ac3|proper|repack)\b.*$`)
	spaceRe = regexp.MustCompile(`[._]+|\s+`)

	// "Show Name S01E02 Episode Title", "show.name.1x02", "S01E01E02", with
	// an optional "(2011)" after the show name
	episodeRe   = regexp.MustCompile(`(?i)^(?:(.*?)[\s._\-]+)?(?:S(\d{1,2})[\s._\-]*E(\d{1,3})(?:[\s._\-]*E\d{1,3})*|(\d{1,2})x(\d{2,3}))(?:[\s._\-]+(.*))?$`)
	seasonDirRe = regexp.MustCompile(`(?i)^(season|series|s)[\s._\-]*\d+$|^specials$`)
)

type episodeName struct {
	Series  string
	Year    int
	Season  int
	Episode int
	Title   string
}

func parseEpisode(name string) (episodeName, bool) {
	m := episodeRe.FindStringSubmatch(name)
	if m == nil {
		return episodeName{}, false
	}
	var e episodeName
	if m[2] != "" {
		e.Season, _ = strconv.Atoi(m[2])
		e.Episode, _ = strconv.Atoi(m[3])
	} else {
		e.Season, _ = strconv.Atoi(m[4])
		e.Episode, _ = strconv.Atoi(m[5])
	}
	if strings.TrimSpace(m[1]) != "" {
		e.Series, e.Year = parseName(m[1])
	}
	e.Title = cleanEpisodeTitle(m[6])
	return e, true
}

// seriesFromDirs handles "Show Name/Season 01/S01E01.mkv": the show is the
// folder above the season folder (never the library root itself).
func seriesFromDirs(path, root string) (string, int) {
	dir := filepath.Dir(path)
	if seasonDirRe.MatchString(filepath.Base(dir)) {
		dir = filepath.Dir(dir)
	}
	if filepath.Clean(dir) == filepath.Clean(root) {
		return "", 0
	}
	return parseName(filepath.Base(dir))
}

func cleanEpisodeTitle(s string) string {
	s = noiseRe.ReplaceAllString(" "+s, "")
	s = spaceRe.ReplaceAllString(s, " ")
	s = strings.TrimSpace(strings.Trim(s, " -"))
	if s != "" && s == strings.ToLower(s) {
		s = titleCase(s)
	}
	return s
}

// guessTitle prefers "Title (Year)" from the file name, then from the parent
// folder (common layout: "Heat (1995)/heat.mkv"), then falls back to the bare name.
func guessTitle(path, root string) (string, int) {
	t, y := parseName(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	if y != 0 {
		return t, y
	}
	parent := filepath.Dir(path)
	if filepath.Clean(parent) != filepath.Clean(root) {
		if pt, py := parseName(filepath.Base(parent)); py != 0 {
			return pt, py
		}
	}
	return t, 0
}

func parseName(name string) (string, int) {
	title, year := name, 0
	if m := yearRe.FindStringSubmatch(name); m != nil {
		title = m[1]
		year, _ = strconv.Atoi(m[2])
	} else {
		title = noiseRe.ReplaceAllString(title, "")
	}
	title = spaceRe.ReplaceAllString(title, " ")
	title = strings.TrimSpace(strings.Trim(title, " -"))
	if title == "" {
		title = name
	}
	if title == strings.ToLower(title) {
		title = titleCase(title) // "transcode test" -> "Transcode Test"
	}
	return title, year
}

func titleCase(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

func looksLikeExtra(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "sample") || strings.Contains(n, "trailer")
}
