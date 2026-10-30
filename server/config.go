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
	Library       []string // folders to scan
	DataDir       string   // db, posters, transcode segments
	HTTPPort      int
	DiscoveryPort int

	TMDBKey string

	FFmpeg  string // path or "" for PATH lookup
	FFprobe string
	Encoder string // auto | nvenc | qsv | amf | software

	TSAuthKey  string // empty disables tsnet
	TSHostname string
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
	}
	sep := ":"
	if runtime.GOOS == "windows" {
		sep = ";"
	}
	for _, p := range strings.Split(os.Getenv("FLEX_LIBRARY"), sep) {
		if p = strings.TrimSpace(p); p != "" {
			c.Library = append(c.Library, filepath.Clean(p))
		}
	}
	return c, c.Validate()
}

func (c *Config) Validate() error {
	if c.HTTPPort <= 0 || c.HTTPPort > 65535 {
		return fmt.Errorf("invalid HTTP port %d", c.HTTPPort)
	}
	if c.FFmpeg == "" {
		c.FFmpeg = "ffmpeg"
	}
	if c.FFprobe == "" {
		c.FFprobe = "ffprobe"
	}
	switch c.Encoder {
	case "auto", "nvenc", "qsv", "amf", "software":
	default:
		return fmt.Errorf("unknown encoder %q", c.Encoder)
	}
	return nil
}

func (c Config) DBPath() string        { return filepath.Join(c.DataDir, "flex.db") }
func (c Config) PosterDir() string     { return filepath.Join(c.DataDir, "posters") }
func (c Config) TranscodeDir() string  { return filepath.Join(c.DataDir, "transcode") }

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
