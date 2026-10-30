package server

import (
	"context"
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
// the movies table in sync. One scan at a time.
type Scanner struct {
	cfg  Config
	db   *DB
	tmdb *TMDB // nil when no API key is configured
	log  *slog.Logger

	mu     sync.Mutex
	status ScanStatus
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
	now := time.Now()
	size, mtime := info.Size(), info.ModTime().Unix()

	s.mu.Lock()
	s.status.FilesSeen++
	s.mu.Unlock()

	existing, err := s.db.LookupFile(path)
	if err != nil {
		return err
	}
	if existing != nil && existing.Size == size && existing.MTime == mtime {
		if existing.TMDBID == 0 && s.tmdb != nil {
			s.enrich(ctx, existing.ID, path, root)
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
		s.enrich(ctx, id, path, root)
	}
	return nil
}

func (s *Scanner) enrich(ctx context.Context, id int64, path, root string) {
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

// --- filename parsing ---

var (
	yearRe  = regexp.MustCompile(`^(.+?)[\s._\-(\[]+((?:19|20)\d{2})(?:[\s._\-)\]]|$)`)
	noiseRe = regexp.MustCompile(`(?i)[\s._\-](1080p|2160p|720p|480p|4k|uhd|bluray|blu-ray|bdrip|brrip|web-?dl|webrip|hdtv|dvdrip|x264|x265|h\.?264|h\.?265|hevc|remux|hdr|dv|atmos|dts|aac|ac3|proper|repack)\b.*$`)
	spaceRe = regexp.MustCompile(`[._]+|\s+`)
)

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
	return title, year
}

func looksLikeExtra(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "sample") || strings.Contains(n, "trailer")
}
