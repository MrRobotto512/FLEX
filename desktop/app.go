package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"flex/server"
	"flex/web"
)

// Settings is what the UI edits; it maps 1:1 onto server.Config and is
// persisted as JSON in %APPDATA%\FLEX\settings.json.
type Settings struct {
	Name            string   `json:"name"`
	Library         []string `json:"library"`
	TVLibrary       []string `json:"tv_library"`
	DataDir         string   `json:"data_dir"`
	HTTPPort        int      `json:"http_port"`
	DiscoveryPort   int      `json:"discovery_port"`
	TMDBKey         string   `json:"tmdb_key"`
	FFmpeg          string   `json:"ffmpeg"`
	FFprobe         string   `json:"ffprobe"`
	Encoder         string   `json:"encoder"`
	TSEnabled       bool     `json:"ts_enabled"`
	TSAuthKey       string   `json:"ts_authkey"`
	TSHostname      string   `json:"ts_hostname"`
	PS5KeepLauncher bool     `json:"ps5_keep_launcher"`
}

type Status struct {
	Running    bool                 `json:"running"`
	Error      string               `json:"error,omitempty"`
	LANAddrs   []string             `json:"lan_addrs"`
	Scan       server.ScanStatus    `json:"scan"`
	Autostart  bool                 `json:"autostart"`
	Version    string               `json:"version"`
	Encoder    string               `json:"encoder"`
	Transcodes []server.SessionInfo `json:"transcodes"`

	TSEnabled    bool   `json:"ts_enabled"`
	TSState      string `json:"ts_state"`
	TSDNS        string `json:"ts_dns"`
	TSIP         string `json:"ts_ip"`
	TSNeedsLogin bool   `json:"ts_needs_login"`
	TSError      string `json:"ts_error,omitempty"`
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
	a.startTray()
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
	st := Status{
		Running:   a.srv != nil,
		Error:     a.runErr,
		LANAddrs:  lanAddrs(a.settings.HTTPPort),
		Autostart: autostartEnabled(),
		Version:   server.Version,
		TSEnabled: a.settings.TSEnabled || a.settings.TSAuthKey != "",
		TSState:   "Off",
	}
	if a.srv != nil {
		st.Scan = a.srv.ScanStatus()
		st.Encoder = a.srv.Encoder()
		st.Transcodes = a.srv.Transcodes()
		ts := a.srv.TailscaleInfo()
		st.TSState, st.TSDNS, st.TSIP, st.TSError = ts.State, ts.DNSName, ts.IP, ts.Error
		st.TSNeedsLogin = ts.State == "NeedsLogin" && ts.AuthURL != ""
	}
	return st
}

// TailscaleSignIn opens the Tailscale login page for this server's node.
func (a *App) TailscaleSignIn() {
	a.mu.Lock()
	srv := a.srv
	a.mu.Unlock()
	if srv == nil {
		return
	}
	if url := srv.TailscaleInfo().AuthURL; url != "" {
		runtime.BrowserOpenURL(a.ctx, url)
	}
}

func (a *App) SetAutostart(on bool) error { return setAutostart(on) }

func (a *App) PickFolder(title string) (string, error) {
	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{Title: title})
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
		Name: s.Name, Library: s.Library, TVLibrary: s.TVLibrary, DataDir: s.DataDir,
		HTTPPort: s.HTTPPort, DiscoveryPort: s.DiscoveryPort, TMDBKey: s.TMDBKey,
		FFmpeg: s.FFmpeg, FFprobe: s.FFprobe, Encoder: s.Encoder,
		TSEnabled: s.TSEnabled, TSAuthKey: s.TSAuthKey, TSHostname: s.TSHostname,
		PS5KeepLauncher: s.PS5KeepLauncher,
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
		PS5KeepLauncher: true,
	}
}

func loadSettings() Settings {
	s := defaultSettings() // fields missing from an older settings.json keep these defaults
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

// lanAddrs lists the URLs a client on the same network can use, skipping
// virtual adapters (WSL, Hyper-V, Bluetooth) that only confuse the list.
func lanAddrs(port int) []string {
	var out []string
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		name := strings.ToLower(i.Name)
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 ||
			strings.Contains(name, "vethernet") || strings.Contains(name, "bluetooth") || strings.Contains(name, "tailscale") {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLinkLocalUnicast() {
				out = append(out, fmt.Sprintf("http://%s:%d", ipn.IP, port))
			}
		}
	}
	return out
}
