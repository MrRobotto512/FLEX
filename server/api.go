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
	mux.HandleFunc("GET /api/v1/movies/{id}/poster", s.handleImage("poster"))
	mux.HandleFunc("GET /api/v1/movies/{id}/backdrop", s.handleImage("backdrop"))
	mux.HandleFunc("GET /api/v1/movies/{id}/play", s.handlePlay)
	mux.HandleFunc("GET /api/v1/movies/{id}/file", s.handleFile)
	mux.HandleFunc("PUT /api/v1/movies/{id}/progress", s.handleProgress)
	mux.HandleFunc("POST /api/v1/movies/{id}/match", s.handleMatch)
	mux.HandleFunc("GET /api/v1/hls/{session}/{file}", s.handleHLS)
	mux.HandleFunc("DELETE /api/v1/hls/{session}", s.handleHLSStop)
	mux.HandleFunc("POST /api/v1/library/scan", s.handleScan)
	mux.HandleFunc("GET /api/v1/library/status", s.handleScanStatus)
	mux.HandleFunc("GET /api/v1/library/folders", s.handleFoldersGet)
	mux.HandleFunc("PUT /api/v1/library/folders", s.handleFoldersPut)
	if s.web != nil {
		mux.Handle("/", http.FileServerFS(s.web))
	}
	return logging(s.log, cors(mux))
}

func (s *Server) handleServer(w http.ResponseWriter, r *http.Request) {
	features := []string{"direct", "hls"}
	s.mu.Lock()
	if s.tsUp {
		features = append(features, "tsnet")
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, ServerInfo{
		Name: s.cfg.Name, ID: s.id, Version: Version, Features: features,
		Remote: isTailnetAddr(r.RemoteAddr),
	})
}

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

func (s *Server) handleImage(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m, ok := s.movie(w, r)
		if !ok {
			return
		}
		path := m.posterPath
		if kind == "backdrop" {
			path = m.backdropPath
		}
		if path == "" {
			writeErr(w, http.StatusNotFound, "not_found", "no "+kind)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeFile(w, r, path)
	}
}

func (s *Server) handlePlay(w http.ResponseWriter, r *http.Request) {
	m, ok := s.movie(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	profile := q.Get("profile")
	if profile == "" {
		switch {
		case r.Header.Get("X-Flex-Client") == "ps5":
			profile = "ps5"
		case isTailnetAddr(r.RemoteAddr):
			profile = "remote"
		default:
			profile = "direct"
		}
	}
	audio := intOr(q.Get("audio"), -1)
	sub := intOr(q.Get("sub"), -1)
	start, _ := strconv.ParseFloat(q.Get("start"), 64)

	info, err := s.decidePlay(r.Context(), m, profile, audio, sub, start)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "ffmpeg_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	m, ok := s.movie(w, r)
	if !ok {
		return
	}
	serveFile(w, r, m.Path)
}

func (s *Server) handleProgress(w http.ResponseWriter, r *http.Request) {
	m, ok := s.movie(w, r)
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
	if err := s.db.SetProgress(m.ID, body.PositionS, body.DurationS); err != nil {
		writeErr(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

// --- helpers ---

func (s *Server) movie(w http.ResponseWriter, r *http.Request) (*Movie, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "bad movie id")
		return nil, false
	}
	m, err := s.db.GetMovie(id)
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "movie "+r.PathValue("id")+" does not exist")
		return nil, false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", err.Error())
		return nil, false
	}
	return m, true
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
