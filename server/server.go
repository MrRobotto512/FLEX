package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Server ties the pieces together. The desktop app and flexd both construct
// one and call Run.
type Server struct {
	cfg     Config
	db      *DB
	tmdb    *TMDB
	scanner *Scanner
	tc      *Transcoder
	id      string
	log     *slog.Logger
	web     fs.FS // static UI, may be nil

	mu      sync.Mutex
	folders []string
	ctx     context.Context
	tsUp    bool
}

func New(cfg Config, log *slog.Logger, web fs.FS) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	db, err := OpenDB(cfg.DBPath())
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	id, err := db.GetSetting("server_id")
	if err != nil {
		return nil, err
	}
	if id == "" {
		b := make([]byte, 8)
		rand.Read(b)
		id = hex.EncodeToString(b)
		if err := db.SetSetting("server_id", id); err != nil {
			return nil, err
		}
	}

	s := &Server{cfg: cfg, db: db, id: id, log: log, web: web}

	s.folders = cfg.Library
	if len(s.folders) == 0 {
		if raw, _ := db.GetSetting("library"); raw != "" {
			json.Unmarshal([]byte(raw), &s.folders)
		}
	}

	if cfg.TMDBKey != "" {
		s.tmdb = NewTMDB(cfg.TMDBKey, cfg.PosterDir(), log)
	} else {
		log.Warn("no FLEX_TMDB_KEY set; movies will have no posters or descriptions")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	encoder := DetectEncoder(ctx, cfg.FFmpeg, cfg.Encoder)
	cancel()
	log.Info("video encoder", "encoder", encoder)
	s.tc = NewTranscoder(cfg.FFmpeg, encoder, cfg.TranscodeDir(), log)
	s.scanner = NewScanner(cfg, db, s.tmdb, log)
	return s, nil
}

// Run serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()

	handler := s.Handler()
	httpSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", s.cfg.HTTPPort),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errc := make(chan error, 2)
	go func() { errc <- httpSrv.ListenAndServe() }()
	go func() { errc <- runDiscovery(ctx, s.cfg.DiscoveryPort, s.discoveryReply(), s.log) }()
	go s.tc.Reap(ctx, 5*time.Minute)
	s.log.Info("FLEX listening", "http", s.cfg.HTTPPort, "name", s.cfg.Name, "id", s.id)

	if s.cfg.TSAuthKey != "" {
		ts, err := s.startTailscale(ctx, handler)
		if err != nil {
			s.log.Error("tailscale", "err", err)
		} else {
			defer ts.Close()
		}
	}

	if folders := s.Folders(); len(folders) > 0 {
		s.scanner.Start(ctx, folders)
	} else {
		s.log.Warn("no library folders configured")
	}

	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpSrv.Shutdown(shutCtx)
	s.db.Close()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) Folders() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.folders...)
}

func (s *Server) SetFolders(folders []string) error {
	raw, _ := json.Marshal(folders)
	if err := s.db.SetSetting("library", string(raw)); err != nil {
		return err
	}
	s.mu.Lock()
	s.folders = folders
	s.mu.Unlock()
	return nil
}

// Scan starts a library scan; false if one is already running.
func (s *Server) Scan() bool { return s.scanner.Start(s.bg(), s.Folders()) }

func (s *Server) ScanStatus() ScanStatus { return s.scanner.Status() }

// TailscaleUp reports whether the tsnet listener came up.
func (s *Server) TailscaleUp() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tsUp
}

// Config returns the configuration the server was started with.
func (s *Server) Config() Config { return s.cfg }

func (s *Server) bg() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx == nil {
		return context.Background()
	}
	return s.ctx
}

func (s *Server) discoveryReply() []byte {
	b, _ := json.Marshal(map[string]any{
		"name": s.cfg.Name, "id": s.id, "port": s.cfg.HTTPPort, "version": Version,
	})
	return b
}
