package server

import "time"

// Movie is one library entry as the API returns it. Path never leaves the
// server; Media is only filled for the detail endpoint.
type Movie struct {
	ID       int64     `json:"id"`
	Title    string    `json:"title"`
	Year     int       `json:"year,omitempty"`
	RuntimeS int       `json:"runtime_s,omitempty"`
	Overview string    `json:"overview,omitempty"`
	Poster   string    `json:"poster,omitempty"`
	Backdrop string    `json:"backdrop,omitempty"`
	ResumeS  int       `json:"resume_s"`
	Watched  bool      `json:"watched"`
	AddedAt  time.Time `json:"added_at"`
	TMDBID   int64     `json:"tmdb_id,omitempty"`

	Media *MediaInfo `json:"media,omitempty"`

	Path         string `json:"-"`
	posterPath   string
	backdropPath string
}

// MediaInfo is what ffprobe told us about the file.
type MediaInfo struct {
	Container string      `json:"container"`
	Size      int64       `json:"size"`
	DurationS float64     `json:"duration_s"`
	BitRate   int64       `json:"bit_rate,omitempty"`
	Video     VideoInfo   `json:"video"`
	Audio     []AudioInfo `json:"audio"`
	Subtitles []SubInfo   `json:"subtitles"`
}

type VideoInfo struct {
	Index    int     `json:"index"`
	Codec    string  `json:"codec"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	BitDepth int     `json:"bit_depth"`
	HDR      string  `json:"hdr,omitempty"` // "", hdr10, hlg, dolbyvision
	FPS      float64 `json:"fps"`
}

type AudioInfo struct {
	Index    int    `json:"index"` // absolute stream index, usable in ffmpeg -map 0:N
	Codec    string `json:"codec"`
	Channels int    `json:"channels"`
	Lang     string `json:"lang,omitempty"`
	Title    string `json:"title,omitempty"`
	Default  bool   `json:"default"`
}

type SubInfo struct {
	Index  int    `json:"index"`
	Codec  string `json:"codec"`
	Lang   string `json:"lang,omitempty"`
	Title  string `json:"title,omitempty"`
	Forced bool   `json:"forced"`
}

// Show is a TV series; episodes hang off it. SeasonList and NextEpisode are
// only filled by the detail endpoint.
type Show struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	Year      int       `json:"year,omitempty"`
	Overview  string    `json:"overview,omitempty"`
	Poster    string    `json:"poster,omitempty"`
	Backdrop  string    `json:"backdrop,omitempty"`
	Seasons   int       `json:"seasons"`
	Episodes  int       `json:"episodes"`
	Unwatched int       `json:"unwatched"`
	AddedAt   time.Time `json:"added_at"`
	TMDBID    int64     `json:"tmdb_id,omitempty"`

	SeasonList  []Season `json:"season_list,omitempty"`
	NextEpisode *Episode `json:"next_episode,omitempty"`

	posterPath   string
	backdropPath string
}

type Season struct {
	Season   int       `json:"season"`
	Episodes []Episode `json:"episodes"`
}

type Episode struct {
	ID        int64     `json:"id"`
	ShowID    int64     `json:"show_id"`
	ShowTitle string    `json:"show_title,omitempty"`
	Season    int       `json:"season"`
	Episode   int       `json:"episode"`
	Title     string    `json:"title"`
	Overview  string    `json:"overview,omitempty"`
	RuntimeS  int       `json:"runtime_s,omitempty"`
	Still     string    `json:"still,omitempty"`
	ResumeS   int       `json:"resume_s"`
	Watched   bool      `json:"watched"`
	AddedAt   time.Time `json:"added_at"`

	Media *MediaInfo `json:"media,omitempty"`

	Path      string `json:"-"`
	stillPath string
}

// EpisodeMeta is what TMDB knows about one episode.
type EpisodeMeta struct {
	Title     string
	Overview  string
	RuntimeS  int
	StillPath string
}

// Metadata is what a TMDB match (or a manual fix) writes back.
type Metadata struct {
	TMDBID       int64
	Title        string
	Year         int
	Overview     string
	RuntimeS     int
	PosterPath   string // local file under PosterDir, "" if none
	BackdropPath string
}

// PlayInfo answers GET /movies/{id}/play.
type PlayInfo struct {
	Method     string  `json:"method"` // direct | hls
	URL        string  `json:"url"`
	DurationS  float64 `json:"duration_s"`
	Container  string  `json:"container,omitempty"`
	VideoCodec string  `json:"video_codec"`
	AudioCodec string  `json:"audio_codec"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	Upscaled   bool    `json:"upscaled,omitempty"`
	BitRate    int64   `json:"bit_rate,omitempty"` // transcodes: the video bitrate chosen for the link
}

type ScanStatus struct {
	Scanning  bool       `json:"scanning"`
	FilesSeen int        `json:"files_seen"`
	Movies    int        `json:"movies"`
	Shows     int        `json:"shows"`
	Episodes  int        `json:"episodes"`
	LastScan  *time.Time `json:"last_scan,omitempty"`
	Current   string     `json:"current,omitempty"` // what the scan is doing right now
	LastError string     `json:"last_error,omitempty"`
}

type ServerInfo struct {
	Name      string   `json:"name"`
	ID        string   `json:"id"`
	Version   string   `json:"version"`
	Features  []string `json:"features"`
	Remote    bool     `json:"remote"`
	Tailscale *TSInfo  `json:"tailscale,omitempty"`
	Encoder   string   `json:"encoder,omitempty"`
	HDR       string   `json:"hdr_tonemap,omitempty"` // "gpu" (libplacebo) | "cpu"
	// why a hardware encoder was or wasn't used, and the HDR (libplacebo) test
	GPUs         []string          `json:"gpus,omitempty"`
	EncoderTests map[string]string `json:"encoder_tests,omitempty"`
}

// Version is the release this build belongs to; CI sets it from the tag
// (-ldflags "-X flex/server.Version=0.3.0"). Updates compare against it.
var Version = "0.2.0"
