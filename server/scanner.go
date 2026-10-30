package server

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Root is a library folder. In a TV root every video is an episode; in a
// movie root files are movies unless they are named S01E02 / 1x02 or sit
// in a season folder.
type Root struct {
	Path string
	TV   bool
}

// matcherVersion is bumped when title parsing or TMDB matching improves, so
// movies that found no match are tried once more on the next scan (and only
// then).
const matcherVersion = 3

// scanWorkers files are probed and matched at a time: ffprobe on a slow
// drive and TMDB round trips are both mostly waiting.
const scanWorkers = 4

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

	// per-scan caches, guarded by showMu (show creation and TMDB show
	// matching run one at a time so two workers never create the same show)
	showMu  sync.Mutex
	shows   map[string]int64 // show key -> id
	touched map[int64]bool   // shows seen this scan (episode metadata pass)
}

func (s *Scanner) setCurrent(what string) {
	s.mu.Lock()
	s.status.Current = what
	s.mu.Unlock()
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
func (s *Scanner) Start(ctx context.Context, roots []Root) bool {
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
		err := s.scan(ctx, roots)
		now := time.Now()
		s.mu.Lock()
		s.status.Scanning = false
		s.status.Current = ""
		s.status.LastScan = &now
		if err != nil {
			s.status.LastError = err.Error()
		}
		s.mu.Unlock()
	}()
	return true
}

func (s *Scanner) scan(ctx context.Context, roots []Root) error {
	start := time.Now()
	s.shows = map[string]int64{}
	s.touched = map[int64]bool{}
	s.log.Info("scan started", "roots", roots)
	tvRoots := map[string]bool{}
	for i, r := range roots {
		// a folder called "TV Shows" in the movie list is a TV folder
		if !r.TV && tvRootNameRe.MatchString(filepath.Base(filepath.Clean(r.Path))) {
			s.log.Info("scan: treating as a TV folder", "path", r.Path)
			roots[i].TV = true
			r.TV = true
		}
		if r.TV {
			tvRoots[strings.ToLower(filepath.Clean(r.Path))] = true
		}
	}
	type job struct {
		root Root
		path string
		d    fs.DirEntry
	}
	jobs := make(chan job, 64)
	var wg sync.WaitGroup
	for i := 0; i < scanWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					continue
				}
				if err := s.file(ctx, j.root, j.path, j.d); err != nil {
					s.log.Warn("scan: file", "path", j.path, "err", err)
				}
			}
		}()
	}

	// a library folder that can't be read (drive unplugged, NAS asleep) must
	// not make everything in it look deleted
	var unreachable []string
	var walkErr error
	for _, root := range roots {
		s.setCurrent("Listing " + root.Path)
		err := filepath.WalkDir(root.Path, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				s.log.Warn("scan: skipping", "path", path, "err", err)
				if filepath.Clean(path) == filepath.Clean(root.Path) {
					unreachable = append(unreachable, root.Path)
				}
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if d.IsDir() {
				if strings.HasPrefix(d.Name(), ".") || (path != root.Path && extrasDirRe.MatchString(d.Name())) {
					return filepath.SkipDir
				}
				// a TV folder inside a movie folder is scanned as TV, once
				if !root.TV && path != root.Path && tvRoots[strings.ToLower(filepath.Clean(path))] {
					return filepath.SkipDir
				}
				return nil
			}
			if !videoExt[strings.ToLower(filepath.Ext(d.Name()))] || looksLikeExtra(d.Name()) {
				return nil
			}
			jobs <- job{root, path, d}
			return nil
		})
		if err != nil {
			walkErr = err
			break
		}
	}
	close(jobs)
	wg.Wait()
	if walkErr != nil {
		return walkErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s.setCurrent("Merging shows")
	if n, err := s.db.MergeDuplicateShows(); err != nil {
		s.log.Warn("scan: merging shows", "err", err)
	} else if n > 0 {
		s.log.Info("scan: merged duplicate shows", "count", n)
	}
	s.episodeMetadata(ctx)
	s.setCurrent("Cleaning up")
	removed, err := s.db.DeleteUnseen(start, unreachable)
	if err != nil {
		return err
	}
	s.log.Info("scan finished", "removed", removed, "took", time.Since(start).Round(time.Millisecond))
	return nil
}

func (s *Scanner) file(ctx context.Context, root Root, path string, d fs.DirEntry) error {
	info, err := d.Info()
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.status.FilesSeen++
	s.mu.Unlock()

	size, mtime := info.Size(), info.ModTime().Unix()
	if root.TV || inSeasonFolder(path, root.Path) {
		return s.episodeFile(ctx, root.Path, path, size, mtime, parseTVEpisode(path, root.Path))
	}
	base := strings.TrimSuffix(d.Name(), filepath.Ext(d.Name()))
	if ep, ok := parseEpisode(base); ok {
		return s.episodeFile(ctx, root.Path, path, size, mtime, ep)
	}
	return s.movieFile(ctx, root.Path, path, size, mtime)
}

func (s *Scanner) movieFile(ctx context.Context, root, path string, size, mtime int64) error {
	now := time.Now()
	existing, err := s.db.LookupFile(path)
	if err != nil {
		return err
	}
	if existing != nil && existing.Size == size && existing.MTime == mtime {
		if !existing.HasMeta && existing.MatchVer < matcherVersion && s.tmdb != nil {
			s.enrichMovie(ctx, existing.ID, path, root)
		}
		return s.db.TouchSeen(existing.ID, now)
	}

	s.setCurrent("Reading " + filepath.Base(path))
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
	type try struct {
		title string
		year  int
	}
	var tries []try
	for _, c := range titleCandidates(title) {
		tries = append(tries, try{c, year})
	}
	// the folder often says it better than the file ("Zoolander.2001.BluRay…/
	// www.HDSector.com - Zoolander…mkv")
	if parent := filepath.Dir(path); filepath.Clean(parent) != filepath.Clean(root) {
		ft, fy := parseName(filepath.Base(parent))
		if fy == 0 {
			fy = year
		}
		for _, c := range titleCandidates(ft)[:min(3, len(titleCandidates(ft)))] {
			tries = append(tries, try{c, fy})
		}
	}
	s.setCurrent("Looking up " + title + " on TMDB")
	var md Metadata
	var err error
	for _, t := range tries {
		if md, err = s.tmdb.Match(ctx, t.title, t.year); err == nil {
			break
		}
	}
	s.db.SetMatchVersion(id, matcherVersion)
	if err != nil {
		s.db.SetTitle(id, title, year) // at least show the cleaned-up name
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
	s.showMu.Lock()
	showID, ok := s.shows[key]
	if !ok {
		s.setCurrent("Looking up " + series + " on TMDB")
		id, created, err := s.db.UpsertShow(key, series, year, now)
		if err != nil {
			s.showMu.Unlock()
			return err
		}
		showID = id
		s.shows[key] = id
		if created {
			s.log.Info("scan: new show", "title", series, "year", year)
		}
		if s.tmdb != nil && (created || s.db.ShowNeedsMetadata(id)) {
			var md Metadata
			var err error
			for _, c := range titleCandidates(series) {
				if md, err = s.tmdb.MatchShow(ctx, c, year); err == nil {
					break
				}
			}
			if err != nil {
				s.log.Warn("tmdb: no show match", "title", series, "year", year, "err", err)
			} else if other := s.db.ShowByTMDB(md.TMDBID, id); other != 0 {
				// same series under another folder name: one show
				s.log.Info("scan: merging show", "folder", series, "into", other)
				if err := s.db.MergeShow(id, other); err == nil {
					showID = other
					s.shows[key] = other
				}
			} else if err := s.db.SetShowMetadata(id, md); err != nil {
				s.log.Warn("tmdb: save show", "id", id, "err", err)
			}
		}
	}
	s.touched[showID] = true
	s.showMu.Unlock()

	existing, err := s.db.LookupEpisodeFile(path)
	if err != nil {
		return err
	}
	title := ep.Title
	if title == "" {
		title = fmt.Sprintf("Episode %d", ep.Episode)
	}
	if existing != nil && (existing.ShowID != showID || existing.Season != ep.Season || existing.Episode != ep.Episode) {
		if err := s.db.PlaceEpisode(existing.ID, showID, ep.Season, ep.Episode, title); err != nil {
			return err
		}
	}
	if existing != nil && existing.Size == size && existing.MTime == mtime {
		return s.db.TouchEpisodeSeen(existing.ID, now)
	}
	s.setCurrent("Reading " + filepath.Base(path))
	media, err := Probe(ctx, s.cfg.FFprobe, path)
	if err != nil {
		return err
	}
	if existing != nil {
		return s.db.UpdateEpisodeFile(existing.ID, size, mtime, media, now)
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
		s.setCurrent(fmt.Sprintf("Episode details (%d) from TMDB", len(need)))
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
