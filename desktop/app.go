package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"flex/server"
	"flex/web"
)

// Settings is what the UI edits; it maps 1:1 onto server.Config and is
// persisted as JSON in %APPDATA%\FLEX\settings.json.
type Settings struct {
	Name          string   `json:"name"`
	Library       []string `json:"library"`
	DataDir       string   `json:"data_dir"`
	HTTPPort      int      `json:"http_port"`
	DiscoveryPort int      `json:"discovery_port"`
	TMDBKey       string   `json:"tmdb_key"`
	FFmpeg        string   `json:"ffmpeg"`
	FFprobe       string   `json:"ffprobe"`
	Encoder       string   `json:"encoder"`
	TSAuthKey     string   `json:"ts_authkey"`
	TSHostname    string   `json:"ts_hostname"`
}

type Status struct {
	Running   bool              `json:"running"`
	Error     string            `json:"error,omitempty"`
	LANAddrs  []string          `json:"lan_addrs"`
	Tailscale bool              `json:"tailscale"`
	Encoder   string            `json:"encoder"`
	Scan      server.ScanStatus `json:"scan"`
}

type App struct {
	ctx      context.Context
	log      *slog.Logger
	settings Settings

	mu     sync.Mutex
	srv    *server.Server
	cancel context.CancelFunc
	runErr string
}

func NewApp() *App {
	return &App{log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.settings = loadSettings()
	a.startServer()
}

func (a *App) shutdown(ctx context.Context) { a.stopServer() }

// --- bound to the frontend as window.go.main.App.* ---

func (a *App) GetSettings() Settings { return a.settings }

// SaveSettings persists and restarts the server with the new configuration.
func (a *App) SaveSettings(s Settings) error {
	cfg := s.config()
	if err := cfg.Validate(); err != nil {
		return err
	}
	a.settings = s
	if err := saveSettings(s); err != nil {
		return err
	}
	a.stopServer()
	a.startServer()
	return nil
}

func (a *App) GetStatus() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := Status{Running: a.srv != nil, Error: a.runErr, LANAddrs: lanAddrs(a.settings.HTTPPort)}
	if a.srv != nil {
		st.Tailscale = a.srv.TailscaleUp()
		st.Scan = a.srv.ScanStatus()
	}
	return st
}

func (a *App) PickFolder() (string, error) {
	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{Title: "Add a movie folder"})
}

func (a *App) StartScan() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.srv != nil && a.srv.Scan()
}

func (a *App) OpenLibrary() {
	runtime.BrowserOpenURL(a.ctx, fmt.Sprintf("http://localhost:%d", a.settings.HTTPPort))
}

// --- server lifecycle ---

func (a *App) startServer() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.runErr = ""
	srv, err := server.New(a.settings.config(), a.log, web.Files)
	if err != nil {
		a.runErr = err.Error()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.srv, a.cancel = srv, cancel
	go func() {
		if err := srv.Run(ctx); err != nil {
			a.mu.Lock()
			a.runErr = err.Error()
			a.srv = nil
			a.mu.Unlock()
		}
	}()
}

func (a *App) stopServer() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
	}
	a.srv, a.cancel = nil, nil
}

// --- settings persistence ---

func (s Settings) config() server.Config {
	return server.Config{
		Name: s.Name, Library: s.Library, DataDir: s.DataDir,
		HTTPPort: s.HTTPPort, DiscoveryPort: s.DiscoveryPort, TMDBKey: s.TMDBKey,
		FFmpeg: s.FFmpeg, FFprobe: s.FFprobe, Encoder: s.Encoder,
		TSAuthKey: s.TSAuthKey, TSHostname: s.TSHostname,
	}
}

func settingsPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "FLEX", "settings.json")
}

func defaultSettings() Settings {
	host, _ := os.Hostname()
	data, _ := os.UserConfigDir()
	return Settings{
		Name: host, DataDir: filepath.Join(data, "FLEX", "data"),
		HTTPPort: 8484, DiscoveryPort: 7555, Encoder: "auto", TSHostname: "flex",
	}
}

func loadSettings() Settings {
	s := defaultSettings()
	b, err := os.ReadFile(settingsPath())
	if err == nil {
		json.Unmarshal(b, &s)
	}
	return s
}

func saveSettings(s Settings) error {
	p := settingsPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(p, b, 0o600) // holds the TMDB key and Tailscale auth key
}

// lanAddrs lists the URLs a client on the same network can use.
func lanAddrs(port int) []string {
	var out []string
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				out = append(out, fmt.Sprintf("http://%s:%d", ipn.IP, port))
			}
		}
	}
	return out
}
