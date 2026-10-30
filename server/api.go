package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"time"
)

// Handler builds the HTTP API described in docs/api.md plus the static web UI.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/server", s.handleServer)

	mux.HandleFunc("GET /api/v1/movies", s.handleMovies)
	mux.HandleFunc("GET /api/v1/movies/{id}", s.handleMovie)
	mux.HandleFunc("GET /api/v1/movies/{id}/poster", s.handleImage("movie", "poster"))
	mux.HandleFunc("GET /api/v1/movies/{id}/backdrop", s.handleImage("movie", "backdrop"))
	mux.HandleFunc("GET /api/v1/movies/{id}/play", s.handlePlay("movie"))
	mux.HandleFunc("GET /api/v1/movies/{id}/file", s.handleFile("movie"))
	mux.HandleFunc("PUT /api/v1/movies/{id}/progress", s.handleProgress("movie"))
	mux.HandleFunc("POST /api/v1/movies/{id}/match", s.handleMatch)

	mux.HandleFunc("GET /api/v1/shows", s.handleShows)
	mux.HandleFunc("GET /api/v1/shows/{id}", s.handleShow)
	mux.HandleFunc("GET /api/v1/shows/{id}/poster", s.handleImage("show", "poster"))
	mux.HandleFunc("GET /api/v1/shows/{id}/backdrop", s.handleImage("show", "backdrop"))

	mux.HandleFunc("GET /api/v1/episodes/{id}", s.handleEpisode)
	mux.HandleFunc("GET /api/v1/episodes/{id}/still", s.handleImage("episode", "still"))
	mux.HandleFunc("GET /api/v1/episodes/{id}/play", s.handlePlay("episode"))
	mux.HandleFunc("GET /api/v1/episodes/{id}/file", s.handleFile("episode"))
	mux.HandleFunc("PUT /api/v1/episodes/{id}/progress", s.handleProgress("episode"))

	mux.HandleFunc("GET /api/v1/hls/{session}/{file}", s.handleHLS)
	mux.HandleFunc("DELETE /api/v1/hls/{session}", s.handleHLSStop)
	mux.HandleFunc("POST /api/v1/library/scan", s.handleScan)
	mux.HandleFunc("GET /api/v1/library/status", s.handleScanStatus)
	mux.HandleFunc("GET /api/v1/library/folders", s.handleFoldersGet)
	mux.HandleFunc("GET /api/v1/library/unmatched", s.handleUnmatched)
	mux.HandleFunc("PUT /api/v1/library/folders", s.handleFoldersPut)
	if s.web != nil {
		mux.Handle("/", http.FileServerFS(s.web))
	}
	return logging(s.log, cors(s.notePS5(mux)))
}

// notePS5 lets the launcher keeper learn the consoles that use FLEX.
func (s *Server) notePS5(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Flex-Client") == "ps5" {
			s.ps5.note(r.RemoteAddr)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleServer(w http.ResponseWriter, r *http.Request) {
	features := []string{"direct", "hls", "tv", "seek"}
	info := ServerInfo{Name: s.cfg.Name, ID: s.id, Version: Version, Remote: isTailnetAddr(r.RemoteAddr)}
	if ts := s.TailscaleInfo(); ts.State == "Running" {
		features = append(features, "tsnet")
		info.Tailscale = &ts
	}
	info.Features = features
	writeJSON(w, http.StatusOK, info)
}

// ---------------------------------------------------------------- movies

func (s *Server) handleMovies(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	movies, err := s.db.ListMovies(q.Get("sort"), q.Get("q"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, movies)
}

func (s *Server) handleMovie(w http.ResponseWriter, r *http.Request) {
	m, ok := s.movie(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleMatch(w http.ResponseWriter, r *http.Request) {
	m, ok := s.movie(w, r)
	if !ok {
		return
	}
	if s.tmdb == nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "no TMDB key configured")
		return
	}
	var body struct {
		TMDBID int64 `json:"tmdb_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.TMDBID == 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "tmdb_id required")
		return
	}
	md, err := s.tmdb.Fetch(r.Context(), body.TMDBID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "tmdb", err.Error())
		return
	}
	if err := s.db.SetMetadata(m.ID, md); err != nil {
		writeErr(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	m, _ = s.db.GetMovie(m.ID)
	writeJSON(w, http.StatusOK, m)
}

// ---------------------------------------------------------------- shows / episodes

func (s *Server) handleShows(w http.ResponseWriter, r *http.Request) {
	shows, err := s.db.ListShows(r.URL.Query().Get("q"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, shows)
}

func (s *Server) handleShow(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	sh, err := s.db.GetShow(id)
	if !s.found(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, sh)
}

func (s *Server) handleEpisode(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ep, err := s.db.GetEpisode(id)
	if !s.found(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, ep)
}

// ---------------------------------------------------------------- shared item handlers

// playItem is the common view of a movie or an episode for playback.
type playItem struct {
	kind  string
	id    int64
	path  string
	media *MediaInfo
}

func (s *Server) item(w http.ResponseWriter, r *http.Request, kind string) (*playItem, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return nil, false
	}
	switch kind {
	case "movie":
		m, err := s.db.GetMovie(id)
		if !s.found(w, r, err) {
			return nil, false
		}
		return &playItem{kind, m.ID, m.Path, m.Media}, true
	default:
		ep, err := s.db.GetEpisode(id)
		if !s.found(w, r, err) {
			return nil, false
		}
		return &playItem{kind, ep.ID, ep.Path, ep.Media}, true
	}
}

func (s *Server) handlePlay(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		it, ok := s.item(w, r, kind)
		if !ok {
			return
		}
		q := r.URL.Query()
		profile := q.Get("profile")
		remote := isTailnetAddr(r.RemoteAddr)
		switch {
		case profile == "" && r.Header.Get("X-Flex-Client") == "ps5":
			profile = "ps5"
		case profile == "" && !remote:
			profile = "direct"
		}
		// away from home the upload is the limit: same codecs, lower bitrate
		if remote && (profile == "" || profile == "ps5") {
			profile = "remote"
		}
		audio := intOr(q.Get("audio"), -1)
		sub := intOr(q.Get("sub"), -1)
		start, _ := strconv.ParseFloat(q.Get("start"), 64)
		upscale := q.Get("upscale") == "1"

		info, err := s.decidePlay(r.Context(), it.kind, it.id, it.path, it.media, profile, audio, sub, start, upscale)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "ffmpeg_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, info)
	}
}

func (s *Server) handleFile(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		it, ok := s.item(w, r, kind)
		if !ok {
			return
		}
		serveFile(w, r, it.path)
	}
}

func (s *Server) handleProgress(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		it, ok := s.item(w, r, kind)
		if !ok {
			return
		}
		var body struct {
			PositionS int `json:"position_s"`
			DurationS int `json:"duration_s"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		if err := s.db.SetProgress(it.kind, it.id, body.PositionS, body.DurationS); err != nil {
			writeErr(w, http.StatusInternalServerError, "db", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleImage serves a cached TMDB image: movie/show poster or backdrop,
// episode still.
func (s *Server) handleImage(kind, which string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var path string
		var err error
		switch kind {
		case "movie":
			var m *Movie
			if m, err = s.db.GetMovie(id); err == nil {
				path = m.posterPath
				if which == "backdrop" {
					path = m.backdropPath
				}
			}
		case "show":
			var sh *Show
			if sh, err = s.db.GetShow(id); err == nil {
				path = sh.posterPath
				if which == "backdrop" {
					path = sh.backdropPath
				}
			}
		default:
			var ep *Episode
			if ep, err = s.db.GetEpisode(id); err == nil {
				path = ep.stillPath
			}
		}
		if !s.found(w, r, err) {
			return
		}
		if path == "" {
			writeErr(w, http.StatusNotFound, "not_found", "no "+which)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeFile(w, r, path)
	}
}

// ---------------------------------------------------------------- transcode / library

func (s *Server) handleHLS(w http.ResponseWriter, r *http.Request) {
	s.tc.Serve(w, r, r.PathValue("session"), r.PathValue("file"))
}

func (s *Server) handleHLSStop(w http.ResponseWriter, r *http.Request) {
	s.tc.Stop(r.PathValue("session"))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	if !s.Scan() {
		writeErr(w, http.StatusConflict, "scan_running", "a scan is already running")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleScanStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.ScanStatus())
}

// handleUnmatched lists movies and shows TMDB could not identify, with their
// file paths, to see why. Home network only: paths stay off the tailnet.
func (s *Server) handleUnmatched(w http.ResponseWriter, r *http.Request) {
	if isTailnetAddr(r.RemoteAddr) {
		writeErr(w, http.StatusForbidden, "forbidden", "only available on the home network")
		return
	}
	movies, shows, err := s.db.Unmatched(intOr(r.URL.Query().Get("limit"), 1000))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"movies": movies, "shows": shows})
}

func (s *Server) handleFoldersGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Folders())
}

func (s *Server) handleFoldersPut(w http.ResponseWriter, r *http.Request) {
	var folders []string
	if err := json.NewDecoder(r.Body).Decode(&folders); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := s.SetFolders(folders); err != nil {
		writeErr(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- helpers

func (s *Server) movie(w http.ResponseWriter, r *http.Request) (*Movie, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return nil, false
	}
	m, err := s.db.GetMovie(id)
	if !s.found(w, r, err) {
		return nil, false
	}
	return m, true
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "bad id")
		return 0, false
	}
	return id, true
}

// found turns a lookup error into the right HTTP response; true means go on.
func (s *Server) found(w http.ResponseWriter, r *http.Request, err error) bool {
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "id "+r.PathValue("id")+" does not exist")
		return false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}

func intOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

var tailnetRanges = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
}

func isTailnetAddr(remoteAddr string) bool {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return false
	}
	for _, p := range tailnetRanges {
		if p.Contains(ap.Addr()) {
			return true
		}
	}
	return false
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, PUT, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Flex-Client, X-Flex-Pin")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func logging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Debug("http", "method", r.Method, "path", r.URL.Path, "from", r.RemoteAddr,
			"client", r.Header.Get("X-Flex-Client"), "took", time.Since(start).Round(time.Millisecond))
	})
}
