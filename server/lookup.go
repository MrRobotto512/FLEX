package server

// Posters for titles that aren't in the library: the PS5 app plays files
// from a USB drive plugged into the console, knows only their names, and
// asks the server (which has the TMDB key) for the artwork.

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type lookupResult struct {
	poster string // local file, "" when there was no match
	at     time.Time
}

type lookups struct {
	mu   sync.Mutex
	seen map[string]lookupResult
}

// lookupPoster finds the poster for a movie file or folder name ("Ambulance
// 2022", "Heat (1995)", "Ambulance.2022.1080p.WEBRip.x264-RARBG.mp4") or a
// show folder name. Results are remembered while the server runs; a miss is
// tried again after an hour (TMDB may have been unreachable).
func (s *Server) lookupPoster(ctx context.Context, kind, name string) string {
	key := kind + "\x00" + strings.ToLower(name)
	s.lookups.mu.Lock()
	if s.lookups.seen == nil {
		s.lookups.seen = map[string]lookupResult{}
	}
	res, ok := s.lookups.seen[key]
	s.lookups.mu.Unlock()
	if ok && (res.poster != "" || time.Since(res.at) < time.Hour) {
		return res.poster
	}
	var md Metadata
	var err error
	if kind == "show" {
		title, year := cleanShowName(name)
		if title == "" {
			title = name
		}
		md, err = s.tmdb.MatchShow(ctx, title, year, EpisodeHint{})
	} else {
		title, year := parseName(strings.TrimSuffix(name, videoSuffix(name)))
		md, err = s.tmdb.Match(ctx, title, year)
	}
	if err != nil {
		s.log.Info("lookup: no poster", "kind", kind, "name", name, "err", err)
	}
	res = lookupResult{poster: md.PosterPath, at: time.Now()}
	s.lookups.mu.Lock()
	s.lookups.seen[key] = res
	s.lookups.mu.Unlock()
	return res.poster
}

// videoSuffix is name's extension when it is a video file's, else "" (so the
// year in "Ambulance.2022" isn't taken for one).
func videoSuffix(name string) string {
	ext := filepath.Ext(name)
	if videoExt[strings.ToLower(ext)] {
		return ext
	}
	return ""
}

// GET /api/v1/lookup/poster?kind=movie|show&name=...: the poster image.
func (s *Server) handleLookupPoster(w http.ResponseWriter, r *http.Request) {
	kind, name := r.URL.Query().Get("kind"), strings.TrimSpace(r.URL.Query().Get("name"))
	if (kind != "movie" && kind != "show") || name == "" || len(name) > 300 {
		writeErr(w, http.StatusBadRequest, "bad_request", "kind=movie|show and name are required")
		return
	}
	if s.tmdb == nil {
		writeErr(w, http.StatusNotFound, "no_tmdb", "this server has no TMDB key")
		return
	}
	poster := s.lookupPoster(r.Context(), kind, name)
	if poster == "" {
		writeErr(w, http.StatusNotFound, "not_found", "no poster found for that name")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, poster)
}
