package server

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Config is everything the server needs to start. The desktop app fills it
// from its settings UI; flexd fills it from the environment / .env.
type Config struct {
	Name          string   // shown to clients
	Library       []string // movie folders (episodes in them are still recognised)
	TVLibrary     []string // TV folders: every video inside is an episode
	DataDir       string   // db, posters, transcode segments
	HTTPPort      int
	DiscoveryPort int

	TMDBKey string

	FFmpeg  string // path or "" for PATH lookup
	FFprobe string
	Encoder string // auto | nvenc | qsv | amf | mf | software

	// Remote access: TSEnabled joins the tailnet; without an auth key the
	// node waits for an interactive sign-in (TailscaleInfo().AuthURL).
	TSEnabled  bool
	TSAuthKey  string
	TSHostname string

	// PS5KeepLauncher re-sends websrv (the homebrew launcher) to consoles
	// that used FLEX once their elfldr is back after a reboot. PS5Hosts adds
	// consoles by address in addition to the ones learned from requests.
	PS5KeepLauncher bool
	PS5Hosts        []string

	// NoUpdates stops the server from checking GitHub for new releases (and
	// so from fetching new PS5 client builds for consoles).
	NoUpdates bool
}

// FromEnv reads FLEX_* variables. Call godotenv.Load first if you want .env.
func FromEnv() (Config, error) {
	c := Config{
		Name:          envOr("FLEX_NAME", hostname()),
		DataDir:       envOr("FLEX_DATA", "./data"),
		HTTPPort:      envInt("FLEX_HTTP_PORT", 8484),
		DiscoveryPort: envInt("FLEX_DISCOVERY_PORT", 7555),
		TMDBKey:       os.Getenv("FLEX_TMDB_KEY"),
		FFmpeg:        os.Getenv("FLEX_FFMPEG"),
		FFprobe:       os.Getenv("FLEX_FFPROBE"),
		Encoder:       envOr("FLEX_ENCODER", "auto"),
		TSAuthKey:     os.Getenv("FLEX_TS_AUTHKEY"),
		TSHostname:    envOr("FLEX_TS_HOSTNAME", "flex"),

		PS5KeepLauncher: envBool("FLEX_PS5_KEEP_LAUNCHER", true),
		NoUpdates:       envBool("FLEX_NO_UPDATES", false),
	}
	c.TSEnabled = envBool("FLEX_TS_ENABLE", false) || c.TSAuthKey != ""
	sep := ":"
	if runtime.GOOS == "windows" {
		sep = ";"
	}
	for _, p := range strings.Split(os.Getenv("FLEX_LIBRARY"), sep) {
		if p = strings.TrimSpace(p); p != "" {
			c.Library = append(c.Library, filepath.Clean(p))
		}
	}
	for _, p := range strings.Split(os.Getenv("FLEX_TV_LIBRARY"), sep) {
		if p = strings.TrimSpace(p); p != "" {
			c.TVLibrary = append(c.TVLibrary, filepath.Clean(p))
		}
	}
	for _, h := range strings.Split(os.Getenv("FLEX_PS5_HOSTS"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			c.PS5Hosts = append(c.PS5Hosts, h)
		}
	}
	return c, c.Validate()
}

func envBool(key string, def bool) bool {
	if v, err := strconv.ParseBool(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func (c *Config) Validate() error {
	if c.HTTPPort <= 0 || c.HTTPPort > 65535 {
		return fmt.Errorf("invalid HTTP port %d", c.HTTPPort)
	}
	// Portable installs ship ffmpeg next to the executable; else the copy
	// EnsureFFmpeg downloaded; else PATH.
	if c.FFmpeg == "" {
		c.FFmpeg = findTool("ffmpeg", c.DataDir)
	}
	if c.FFprobe == "" {
		c.FFprobe = findTool("ffprobe", c.DataDir)
	}
	switch c.Encoder {
	case "auto", "nvenc", "qsv", "amf", "mf", "software":
	default:
		return fmt.Errorf("unknown encoder %q", c.Encoder)
	}
	return nil
}

func (c Config) DBPath() string       { return filepath.Join(c.DataDir, "flex.db") }
func (c Config) PosterDir() string    { return filepath.Join(c.DataDir, "posters") }
func (c Config) TranscodeDir() string { return filepath.Join(c.DataDir, "transcode") }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "FLEX"
	}
	return h
}
