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

	ps5   *ps5Keeper
	ps5up *ps5Updates // newest PS5 client build, offered to consoles
	peers *peers      // other FLEX servers whose libraries this one lists; nil when off

	gpus         []string          // graphics adapters Windows reports
	encoderTests map[string]string // hardware encoder -> "ok" or why not

	mu      sync.Mutex
	folders []string
	ctx     context.Context
	ts      TSInfo
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

	s := &Server{cfg: cfg, db: db, id: id, log: log, web: web, ts: TSInfo{State: "Off"}}
	s.ps5 = newPS5Keeper(s)
	s.ps5up = &ps5Updates{s: s}
	s.ps5up.load()
	if !cfg.NoPeers {
		s.peers = newPeers(s)
	}

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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second) // the settings' encoder, then the ladder
	encoder, tests := DetectEncoder(ctx, cfg.FFmpeg, cfg.Encoder)
	cancel()
	s.encoderTests = tests
	s.gpus = detectGPUs()
	log.Info("video encoder", "encoder", encoder, "gpus", s.gpus, "tests", tests)
	s.tc = NewTranscoder(cfg.FFmpeg, encoder, cfg.TranscodeDir(), log)
	// HDR tone mapping on the GPU (libplacebo) when this ffmpeg and card can;
	// otherwise the CPU does it, which is slow for 4K
	ctx, cancel = context.WithTimeout(context.Background(), 25*time.Second)
	hdr := HDRTest(ctx, cfg.FFmpeg, encoder)
	cancel()
	tests["hdr_libplacebo"] = hdr
	s.tc.SetHDRGPU(hdr == "ok")
	log.Info("HDR tone mapping", "on", s.tc.HDRTonemap(), "test", hdr)
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
	go func() { errc <- runDiscovery(ctx, s.cfg.DiscoveryPort, s.discoveryReply, s.log) }()
	go s.tc.Reap(ctx, 5*time.Minute)
	if s.cfg.PS5KeepLauncher {
		go s.ps5.run(ctx)
	}
	if !s.cfg.NoUpdates {
		go s.ps5up.run(ctx)
	}
	if s.peers != nil {
		go s.peers.run(ctx)
	}
	s.log.Info("FLEX listening", "http", s.cfg.HTTPPort, "name", s.cfg.Name, "id", s.id)

	if s.cfg.TSEnabled || s.cfg.TSAuthKey != "" {
		ts, err := s.startTailscale(ctx, handler)
		if err != nil {
			s.log.Error("tailscale", "err", err)
		} else {
			defer ts.Close()
		}
	}

	if roots := s.roots(); len(roots) > 0 {
		s.scanner.Start(ctx, roots)
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
func (s *Server) Scan() bool { return s.scanner.Start(s.bg(), s.roots()) }

// roots is every library folder: movie folders, then TV folders.
func (s *Server) roots() []Root {
	var out []Root
	for _, f := range s.Folders() {
		out = append(out, Root{Path: f})
	}
	for _, f := range s.cfg.TVLibrary {
		out = append(out, Root{Path: f, TV: true})
	}
	return out
}

func (s *Server) ScanStatus() ScanStatus { return s.scanner.Status() }

// Transcodes lists running ffmpeg sessions; Encoder is the one they use.
func (s *Server) Transcodes() []SessionInfo { return s.tc.Sessions() }
func (s *Server) Encoder() string           { return s.tc.Encoder() }

// Peers lists the linked servers (other FLEX servers whose libraries this one
// shows), online or not.
func (s *Server) Peers() []PeerInfo {
	if s.peers == nil {
		return nil
	}
	return s.peers.info()
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

// discoveryReply is built per request so it carries the Tailscale address
// once the node is up; consoles remember it for use away from home.
func (s *Server) discoveryReply() []byte {
	reply := map[string]any{"name": s.cfg.Name, "id": s.id, "port": s.cfg.HTTPPort, "version": Version}
	if ts := s.TailscaleInfo(); ts.State == "Running" && ts.IP != "" {
		reply["ts_ip"] = ts.IP
	}
	b, _ := json.Marshal(reply)
	return b
}
