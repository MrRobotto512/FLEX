package server

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// profile describes the ceiling a client can decode comfortably.
type profile struct {
	Name       string
	MaxHeight  int
	MaxBitrate int64 // bits/s, video
	AudioBits  string
}

var profiles = map[string]profile{
	"ps5":    {Name: "ps5", MaxHeight: 1080, MaxBitrate: 12_000_000, AudioBits: "192k"},
	"remote": {Name: "remote", MaxHeight: 1080, MaxBitrate: 6_000_000, AudioBits: "128k"},
}

// Transcoder owns ffmpeg HLS sessions. A session is one ffmpeg process writing
// segments into its own folder; idle sessions are reaped.
type Transcoder struct {
	ffmpeg  string
	encoder string // h264_nvenc | h264_qsv | h264_amf | libx264
	dir     string
	log     *slog.Logger

	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	key      string
	dir      string
	cmd      *exec.Cmd
	cancel   context.CancelFunc
	lastUsed time.Time
	done     chan struct{}
	err      error
	info     SessionInfo
}

// SessionInfo describes a running transcode for the API and the desktop app.
type SessionInfo struct {
	Key      string    `json:"key"`
	File     string    `json:"file"`
	Start    float64   `json:"start_s"`
	Mode     string    `json:"mode"` // remux | transcode | upscale
	Started  time.Time `json:"started"`
	LastUsed time.Time `json:"last_used"`
}

// Encoder is the H.264 encoder sessions use.
func (t *Transcoder) Encoder() string { return t.encoder }

// Sessions lists the running transcodes.
func (t *Transcoder) Sessions() []SessionInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := []SessionInfo{}
	for _, s := range t.sessions {
		i := s.info
		i.LastUsed = s.lastUsed
		out = append(out, i)
	}
	return out
}

type transcodeRequest struct {
	Path    string
	Media   *MediaInfo
	Profile profile
	Audio   int     // absolute stream index, -1 = default
	Sub     int     // absolute stream index, -1 = none (burned in when set)
	Start   float64 // seconds
	CopyVid bool    // remux video instead of re-encoding
	Upscale bool    // scale up to the profile's height (Lanczos + light sharpening)
	NoGPU   bool    // CPU filters even with NVENC (retry after the GPU pipeline failed)
}

func NewTranscoder(ffmpeg, encoder, dir string, log *slog.Logger) *Transcoder {
	return &Transcoder{ffmpeg: ffmpeg, encoder: encoder, dir: dir, log: log, sessions: map[string]*session{}}
}

// Key identifies a session by everything that changes the output.
func (r transcodeRequest) Key() string {
	h := sha1.Sum([]byte(fmt.Sprintf("%s|%s|%d|%d|%.0f|%v|%v", r.Path, r.Profile.Name, r.Audio, r.Sub, r.Start, r.CopyVid, r.Upscale)))
	return hex.EncodeToString(h[:6])
}

// Start launches ffmpeg (or reuses a live session) and returns once the
// playlist exists, so the client can fetch it immediately.
func (t *Transcoder) Start(ctx context.Context, r transcodeRequest) (string, error) {
	key := r.Key()
	t.mu.Lock()
	if s, ok := t.sessions[key]; ok {
		s.lastUsed = time.Now()
		t.mu.Unlock()
		return key, nil
	}
	dir := filepath.Join(t.dir, key)
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.mu.Unlock()
		return "", err
	}
	cctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(cctx, t.ffmpeg, t.args(r, dir)...)
	s := &session{key: key, dir: dir, cmd: cmd, cancel: cancel, lastUsed: time.Now(), done: make(chan struct{})}
	s.info = SessionInfo{Key: key, File: filepath.Base(r.Path), Start: r.Start, Mode: "transcode", Started: time.Now()}
	if r.CopyVid {
		s.info.Mode = "remux"
	} else if r.Upscale {
		s.info.Mode = "upscale"
	}
	t.sessions[key] = s
	t.mu.Unlock()

	stderr, _ := cmd.StderrPipe()
	t.log.Info("ffmpeg start", "key", key, "gpu", t.useGPU(r), "args", strings.Join(cmd.Args[1:], " "))
	if err := cmd.Start(); err != nil {
		close(s.done) // nothing will ever close it otherwise and remove() waits on it
		t.remove(key)
		return "", fmt.Errorf("start ffmpeg: %w", err)
	}
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			t.log.Debug("ffmpeg", "key", key, "line", sc.Text())
		}
		s.err = cmd.Wait()
		close(s.done)
		if s.err != nil && cctx.Err() == nil {
			t.log.Warn("ffmpeg exited", "key", key, "err", s.err)
		}
	}()

	// wait for the playlist to appear
	playlist := filepath.Join(dir, "index.m3u8")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(playlist); err == nil {
			return key, nil
		}
		select {
		case <-s.done:
			t.remove(key)
			if t.useGPU(r) {
				// e.g. a codec the GPU can't decode: same job with CPU filters
				t.log.Warn("ffmpeg GPU pipeline failed, retrying on the CPU", "key", key, "err", s.err)
				r.NoGPU = true
				return t.Start(ctx, r)
			}
			return "", fmt.Errorf("ffmpeg failed before producing output: %v", s.err)
		case <-ctx.Done():
			t.remove(key)
			return "", ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.remove(key)
	return "", fmt.Errorf("ffmpeg produced no playlist within 30s")
}

func (t *Transcoder) args(r transcodeRequest, dir string) []string {
	a := []string{"-hide_banner", "-loglevel", "warning", "-nostdin"}
	gpu := t.useGPU(r)
	if !r.CopyVid {
		// Decode on the GPU: with NVENC the frames stay on the card through
		// deinterlacing, scaling and encoding (almost no CPU); otherwise
		// -hwaccel auto at least takes decoding off the CPU, falling back to
		// software by itself. Then encode only a little faster than playback
		// after a first minute in one go, instead of converting the whole film
		// flat out while nobody watches.
		if gpu {
			a = append(a, "-hwaccel", "cuda", "-hwaccel_output_format", "cuda")
		} else {
			a = append(a, "-hwaccel", "auto")
		}
		a = append(a, "-readrate", "1.5", "-readrate_initial_burst", "60")
	}
	if r.Start > 0 {
		a = append(a, "-ss", fmt.Sprintf("%.3f", r.Start))
	}
	a = append(a, "-i", r.Path, "-map", fmt.Sprintf("0:%d", r.Media.Video.Index))
	if r.Audio >= 0 {
		a = append(a, "-map", fmt.Sprintf("0:%d", r.Audio))
	} else {
		a = append(a, "-map", "0:a:0?")
	}

	if r.CopyVid {
		a = append(a, "-c:v", "copy")
	} else if gpu {
		scale := fmt.Sprintf("scale_cuda=-2:'min(%d,ih)':format=yuv420p", r.Profile.MaxHeight)
		if r.Upscale {
			scale = fmt.Sprintf("scale_cuda=-2:%d:interp_algo=lanczos:format=yuv420p", r.Profile.MaxHeight)
		}
		a = append(a, "-vf", "bwdif_cuda=mode=send_frame:deint=interlaced,"+scale)
		a = append(a, t.videoCodecArgs(r.Profile)...)
	} else {
		// deinterlace frames flagged interlaced (old TV and DVD rips); progressive
		// frames pass through untouched
		vf := []string{"bwdif=mode=send_frame:deint=interlaced"}
		if r.Upscale {
			vf = append(vf, fmt.Sprintf("scale=-2:%d:flags=lanczos", r.Profile.MaxHeight), "unsharp=5:5:0.5:5:5:0")
		} else {
			vf = append(vf, fmt.Sprintf("scale=-2:'min(%d,ih)'", r.Profile.MaxHeight))
		}
		if r.Media.Video.HDR != "" {
			// needs an ffmpeg built with libzimg (the gyan.dev Windows builds have it)
			vf = append(vf, "zscale=t=linear:npl=100", "format=gbrpf32le", "zscale=p=bt709",
				"tonemap=hable:desat=0", "zscale=t=bt709:m=bt709:r=tv")
		}
		if r.Sub >= 0 {
			si := subOrdinal(r.Media, r.Sub)
			vf = append(vf, fmt.Sprintf("subtitles=%s:si=%d", ffEscape(r.Path), si))
		}
		vf = append(vf, "format=yuv420p")
		a = append(a, "-vf", strings.Join(vf, ","))
		a = append(a, t.videoCodecArgs(r.Profile)...)
	}

	a = append(a, "-c:a", "aac", "-ac", "2", "-b:a", r.Profile.AudioBits, "-ar", "48000")
	a = append(a,
		"-f", "hls", "-hls_time", "4", "-hls_playlist_type", "event",
		"-hls_segment_type", "mpegts", "-hls_flags", "independent_segments",
		"-hls_segment_filename", filepath.Join(dir, "seg%d.ts"),
		filepath.Join(dir, "index.m3u8"))
	return a
}

// useGPU: the all-CUDA pipeline needs NVENC and no CPU-only filters
// (HDR tone mapping, burned-in subtitles).
func (t *Transcoder) useGPU(r transcodeRequest) bool {
	return t.encoder == "h264_nvenc" && !r.CopyVid && !r.NoGPU && r.Media.Video.HDR == "" && r.Sub < 0
}

func (t *Transcoder) videoCodecArgs(p profile) []string {
	br := fmt.Sprintf("%d", p.MaxBitrate)
	common := []string{"-b:v", br, "-maxrate", br, "-bufsize", fmt.Sprintf("%d", p.MaxBitrate*2),
		"-g", "96", "-keyint_min", "96", "-profile:v", "high", "-level", "4.1"}
	switch t.encoder {
	case "h264_nvenc":
		return append([]string{"-c:v", "h264_nvenc", "-preset", "p4", "-rc", "vbr", "-bf", "2"}, common...)
	case "h264_qsv":
		return append([]string{"-c:v", "h264_qsv", "-preset", "medium"}, common...)
	case "h264_amf":
		return append([]string{"-c:v", "h264_amf", "-quality", "balanced"}, common...)
	default:
		return append([]string{"-c:v", "libx264", "-preset", "veryfast", "-tune", "film", "-sc_threshold", "0"}, common...)
	}
}

// Serve hands out playlist/segments for a session and marks it used.
func (t *Transcoder) Serve(w http.ResponseWriter, req *http.Request, key, file string) {
	t.mu.Lock()
	s, ok := t.sessions[key]
	if ok {
		s.lastUsed = time.Now()
	}
	t.mu.Unlock()
	if !ok || strings.ContainsAny(file, `/\`) {
		http.NotFound(w, req)
		return
	}
	if strings.HasSuffix(file, ".m3u8") {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Content-Type", "video/mp2t")
	}
	http.ServeFile(w, req, filepath.Join(s.dir, file))
}

func (t *Transcoder) Stop(key string) { t.remove(key) }

func (t *Transcoder) remove(key string) {
	t.mu.Lock()
	s, ok := t.sessions[key]
	delete(t.sessions, key)
	t.mu.Unlock()
	if !ok {
		return
	}
	s.cancel()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
	}
	os.RemoveAll(s.dir)
	t.log.Info("ffmpeg stopped", "key", key)
}

// Reap kills sessions nobody has touched for `idle`. Run it in a goroutine.
func (t *Transcoder) Reap(ctx context.Context, idle time.Duration) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			t.mu.Lock()
			keys := make([]string, 0, len(t.sessions))
			for k := range t.sessions {
				keys = append(keys, k)
			}
			t.mu.Unlock()
			for _, k := range keys {
				t.remove(k)
			}
			return
		case <-tick.C:
			t.mu.Lock()
			var stale []string
			for k, s := range t.sessions {
				if time.Since(s.lastUsed) > idle {
					stale = append(stale, k)
				}
			}
			t.mu.Unlock()
			for _, k := range stale {
				t.remove(k)
			}
		}
	}
}

// DetectEncoder picks the best H.264 encoder ffmpeg offers on this machine.
// It also reports what each hardware encoder said, so a PC that ends up on
// libx264 shows why (no such GPU, driver too old, ...).
func DetectEncoder(ctx context.Context, ffmpeg, pref string) (string, map[string]string) {
	named := map[string]string{"nvenc": "h264_nvenc", "qsv": "h264_qsv", "amf": "h264_amf", "software": "libx264"}
	if enc, ok := named[pref]; ok && pref != "auto" {
		return enc, map[string]string{enc: "chosen in settings"}
	}
	tests := map[string]string{}
	for _, enc := range []string{"h264_nvenc", "h264_qsv", "h264_amf"} {
		msg := encoderTest(ctx, ffmpeg, enc)
		tests[enc] = msg
		if msg == "ok" {
			return enc, tests
		}
	}
	return "libx264", tests
}

// encoderTest encodes a few black frames with enc and returns "ok" or the
// last thing ffmpeg complained about. ffmpeg lists every hardware encoder it
// was built with even when the GPU isn't there, so a real test encode is
// the only reliable check.
func encoderTest(ctx context.Context, ffmpeg, enc string) string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "lavfi", "-i", "color=black:s=256x256:d=0.2", "-c:v", enc, "-f", "null", "-")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return "ok"
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" && !strings.HasPrefix(l, "Error while") && !strings.HasPrefix(l, "Conversion failed") {
			return l
		}
	}
	return err.Error()
}

// subOrdinal converts an absolute stream index into the Nth subtitle stream,
// which is what the subtitles filter's si= wants.
func subOrdinal(m *MediaInfo, absIndex int) int {
	for i, s := range m.Subtitles {
		if s.Index == absIndex {
			return i
		}
	}
	return 0
}

// ffEscape quotes a path for use inside an ffmpeg filter graph (Windows
// drive colons and backslashes need escaping).
func ffEscape(p string) string {
	p = strings.ReplaceAll(p, `\`, `/`)
	p = strings.ReplaceAll(p, `:`, `\:`)
	p = strings.ReplaceAll(p, `'`, `\'`)
	return "'" + p + "'"
}
