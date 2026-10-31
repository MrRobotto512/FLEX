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
	"regexp"
	"runtime"
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
	encoder string // h264_nvenc | h264_qsv | h264_amf | h264_mf | libx264
	dir     string
	log     *slog.Logger
	hdrGPU  bool // HDRTest passed: libplacebo tone maps and scales HDR on the GPU

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

// SetHDRGPU switches HDR conversion to libplacebo on the GPU (after HDRTest).
func (t *Transcoder) SetHDRGPU(ok bool) { t.hdrGPU = ok }

// HDRTonemap says where HDR is tone mapped: "gpu" (libplacebo) or "cpu" (zscale).
func (t *Transcoder) HDRTonemap() string {
	if t.hdrGPU {
		return "gpu"
	}
	return "cpu"
}

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
	CPUOnly bool    // libx264 whatever the hardware (retry after the hardware encoder failed)
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
	cmd := noWindow(exec.CommandContext(cctx, t.ffmpeg, t.args(r, dir)...))
	s := &session{key: key, dir: dir, cmd: cmd, cancel: cancel, lastUsed: time.Now(), done: make(chan struct{})}
	s.info = SessionInfo{Key: key, File: filepath.Base(r.Path), Start: r.Start, Mode: "transcode", Started: time.Now()}
	if r.CopyVid {
		s.info.Mode = "remux"
	} else if r.Upscale {
		s.info.Mode = "upscale"
	} else if t.hdrOnGPU(r) {
		s.info.Mode = "transcode, HDR on the GPU"
	} else if t.placebo(r) {
		s.info.Mode = "transcode, scaled on the GPU"
	} else if r.Media.Video.HDR != "" {
		s.info.Mode = "transcode, HDR on the CPU"
	}
	t.sessions[key] = s
	t.mu.Unlock()

	stderr, _ := cmd.StderrPipe()
	t.log.Info("ffmpeg start", "key", key, "gpu", t.useGPU(r), "hdr_gpu", t.hdrOnGPU(r), "args", strings.Join(cmd.Args[1:], " "))
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
			if t.useGPU(r) || t.placebo(r) || t.hdrCUDAScale(r) {
				// e.g. a codec the GPU can't decode: same job with CPU filters
				t.log.Warn("ffmpeg GPU pipeline failed, retrying on the CPU", "key", key, "err", s.err)
				r.NoGPU = true
				return t.Start(ctx, r)
			}
			if !r.CopyVid && t.encoderFor(r) != "libx264" {
				// e.g. a picture size the hardware encoder refuses
				t.log.Warn("ffmpeg hardware encoder failed, retrying with libx264", "key", key, "err", s.err)
				r.CPUOnly = true
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
	hdrGPU, cudaScale := t.placebo(r), t.hdrCUDAScale(r)
	if !r.CopyVid {
		// Decode on the GPU: with NVENC the frames stay on the card through
		// deinterlacing, scaling and encoding (almost no CPU); otherwise
		// -hwaccel auto at least takes decoding off the CPU, falling back to
		// software by itself. Then encode only a little faster than playback
		// after a first minute in one go, instead of converting the whole film
		// flat out while nobody watches.
		if hdrGPU {
			// libplacebo runs on Vulkan; decoded frames go through system memory to it
			a = append(a, "-init_hw_device", "vulkan=vk", "-filter_hw_device", "vk", "-hwaccel", "auto")
		} else if gpu || cudaScale {
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
	} else if hdrGPU {
		// one shader pass on the GPU: tone map HDR to SDR (BT.2390, with peak
		// detection; SDR passes through) and scale to the profile's height
		// with EWA Lanczos
		vf := []string{t.placeboFilter(r.Media.Video.Width, r.Media.Video.Height, r.Profile.MaxHeight, t.encoderFor(r))}
		if r.Sub >= 0 {
			vf = append(vf, fmt.Sprintf("subtitles=%s:si=%d", ffEscape(r.Path), subOrdinal(r.Media, r.Sub)))
		}
		a = append(a, "-vf", strings.Join(vf, ","))
		a = append(a, t.videoCodecArgs(r)...)
	} else if gpu {
		scale := fmt.Sprintf("scale_cuda=-2:'min(%d,ih)':format=yuv420p", r.Profile.MaxHeight)
		if r.Upscale {
			scale = fmt.Sprintf("scale_cuda=-2:%d:interp_algo=lanczos:format=yuv420p", r.Profile.MaxHeight)
		}
		a = append(a, "-vf", "bwdif_cuda=mode=send_frame:deint=interlaced,"+scale)
		a = append(a, t.videoCodecArgs(r)...)
	} else {
		// deinterlace frames flagged interlaced (old TV and DVD rips); progressive
		// frames pass through untouched
		vf := []string{"bwdif=mode=send_frame:deint=interlaced"}
		if cudaScale {
			// HDR without libplacebo but with an NVIDIA card: decode and shrink to
			// the profile's height on the GPU, so the CPU tone maps a quarter of a
			// 4K picture
			w, h := fitHeight(r.Media.Video.Width, r.Media.Video.Height, r.Profile.MaxHeight)
			vf = []string{fmt.Sprintf("scale_cuda=%d:%d:format=p010le", w, h), "hwdownload", "format=p010le"}
		} else if r.Upscale {
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
		if t.encoderFor(r) == "h264_mf" {
			vf = append(vf, "format=nv12") // the only layout GPU encoders take through Media Foundation
		} else {
			vf = append(vf, "format=yuv420p")
		}
		a = append(a, "-vf", strings.Join(vf, ","))
		a = append(a, t.videoCodecArgs(r)...)
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
	return t.encoderFor(r) == "h264_nvenc" && !r.CopyVid && !r.NoGPU && r.Media.Video.HDR == "" && r.Sub < 0
}

// hdrOnGPU: HDR tone mapped and scaled by libplacebo (Vulkan), when HDRTest
// passed at startup. Any GPU encoder or libx264 can take its output.
func (t *Transcoder) hdrOnGPU(r transcodeRequest) bool {
	return t.hdrGPU && r.Media.Video.HDR != "" && !r.CopyVid && !r.NoGPU
}

// placebo: the libplacebo pass handles the picture: HDR (tone mapping), and
// any picture that must shrink to the profile's height when the all-CUDA
// pipeline isn't there (no NVENC, e.g. a GTX 10-series card encoding through
// Media Foundation), so a 4K source isn't scaled on the CPU (~1x real time).
func (t *Transcoder) placebo(r transcodeRequest) bool {
	if !t.hdrGPU || r.CopyVid || r.NoGPU || t.useGPU(r) {
		return false
	}
	return r.Media.Video.HDR != "" || (r.Media.Video.Height > r.Profile.MaxHeight && !r.Upscale)
}

// hdrCUDAScale: HDR without libplacebo on an NVIDIA card: CUDA decodes and
// scales, the CPU only tone maps the smaller picture.
func (t *Transcoder) hdrCUDAScale(r transcodeRequest) bool {
	return !t.hdrGPU && r.Media.Video.HDR != "" && t.encoderFor(r) == "h264_nvenc" && !r.CopyVid && !r.NoGPU
}

// fitHeight scales w×h down to at most maxH lines, keeping the shape (even sizes).
func fitHeight(w, h, maxH int) (int, int) {
	if h <= maxH || h <= 0 {
		return w &^ 1, h &^ 1
	}
	return (w*maxH/h + 1) &^ 1, maxH &^ 1
}

// placeboFilter tone maps HDR to SDR BT.709 and scales to at most maxH lines
// in one libplacebo pass; the output is a plain (system memory) picture in
// the layout enc takes.
func (t *Transcoder) placeboFilter(w, h, maxH int, enc string) string {
	w, h = fitHeight(w, h, maxH)
	format := "yuv420p"
	if enc == "h264_mf" {
		format = "nv12"
	}
	return fmt.Sprintf("libplacebo=w=%d:h=%d:downscaler=ewa_lanczos:upscaler=ewa_lanczos:"+
		"tonemapping=bt.2390:colorspace=bt709:color_primaries=bt709:color_trc=bt709:range=tv:format=%s", w, h, format)
}

// HDRTest tone maps half a second of synthetic HDR10 with libplacebo. "ok"
// when this ffmpeg and GPU can do HDR on the GPU (the build needs libplacebo
// and Vulkan: gyan.dev's full build has them, the essentials build doesn't),
// else what went wrong. Nothing is encoded, so a broken encoder (tested on
// its own by DetectEncoder) can't hide whether libplacebo works.
func HDRTest(ctx context.Context, ffmpeg, enc string) string {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	t := &Transcoder{}
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin",
		"-init_hw_device", "vulkan=vk", "-filter_hw_device", "vk",
		"-f", "lavfi", "-i", "testsrc2=s=1280x720:d=0.5,format=yuv420p10le," +
			"setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc",
		"-vf", t.placeboFilter(1280, 720, 360, enc)}
	out, err := noWindow(exec.CommandContext(ctx, ffmpeg, append(args, "-f", "null", "-")...)).CombinedOutput()
	if err == nil {
		return "ok"
	}
	if msg := ffmpegError(string(out)); msg != "" {
		return msg
	}
	return err.Error()
}

func (t *Transcoder) encoderFor(r transcodeRequest) string {
	if r.CPUOnly {
		return "libx264"
	}
	return t.encoder
}

// upscaleMaxBitrate caps upscaled streams: an SD picture blown up to 1080p
// has no fine detail to spend 12 Mbit/s on, and every bit is decoded by the
// PS5's CPU.
const upscaleMaxBitrate = 8_000_000

func (t *Transcoder) videoCodecArgs(r transcodeRequest) []string {
	p := r.Profile
	if r.Upscale && p.MaxBitrate > upscaleMaxBitrate {
		p.MaxBitrate = upscaleMaxBitrate
	}
	br := fmt.Sprintf("%d", p.MaxBitrate)
	rate := []string{"-b:v", br, "-maxrate", br, "-bufsize", fmt.Sprintf("%d", p.MaxBitrate*2),
		"-g", "96", "-keyint_min", "96"}
	common := append(append([]string{}, rate...), "-profile:v", "high", "-level", "4.1")
	switch t.encoderFor(r) {
	case "h264_nvenc":
		return append([]string{"-c:v", "h264_nvenc", "-preset", "p4", "-rc", "vbr", "-bf", "2"}, common...)
	case "h264_qsv":
		return append([]string{"-c:v", "h264_qsv", "-preset", "medium"}, common...)
	case "h264_amf":
		return append([]string{"-c:v", "h264_amf", "-quality", "balanced"}, common...)
	case "h264_mf":
		// Windows' Media Foundation encoder: the graphics driver does the
		// work, so it still runs on the GPU where ffmpeg's NVENC wants a newer
		// driver than the card can get (GTX 10-series and older). It takes the
		// profile as a number (100 = High) and has no level option. Constant
		// bitrate: with pc_vbr NVIDIA's encoder averaged twice -b:v (20.6
		// Mbit/s asked 10.3), more than the link the bitrate was sized for.
		return append([]string{"-c:v", "h264_mf", "-hw_encoding", "1", "-rate_control", "cbr", "-profile:v", "100"}, rate...)
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
	named := map[string]string{"nvenc": "h264_nvenc", "qsv": "h264_qsv", "amf": "h264_amf", "mf": "h264_mf", "software": "libx264"}
	tests := map[string]string{}
	if enc, ok := named[pref]; ok && pref != "auto" {
		// the settings' choice is tried first, but not trusted blindly: a GTX
		// 10-series card set to NVENC fails on every job once ffmpeg needs a
		// newer driver than the card gets, and each job then fell back to the CPU
		if enc == "libx264" {
			return enc, map[string]string{enc: "chosen in settings"}
		}
		msg := encoderTest(ctx, ffmpeg, enc)
		if msg == "ok" {
			return enc, map[string]string{enc: "ok (chosen in settings)"}
		}
		tests[enc] = "chosen in settings, but: " + msg
	}
	ladder := []string{"h264_nvenc", "h264_qsv", "h264_amf"}
	if runtime.GOOS == "windows" {
		// last resort before the CPU: any GPU whose driver ships an encoder
		ladder = append(ladder, "h264_mf")
	}
	for _, enc := range ladder {
		if _, tried := tests[enc]; tried {
			continue
		}
		msg := encoderTest(ctx, ffmpeg, enc)
		tests[enc] = msg
		if msg == "ok" {
			return enc, tests
		}
	}
	return "libx264", tests
}

// encoderTest encodes a few black frames with enc and returns "ok" or what
// ffmpeg complained about. ffmpeg lists every hardware encoder it was built
// with even when the GPU isn't there, so a real test encode is the only
// reliable check.
func encoderTest(ctx context.Context, ffmpeg, enc string) string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "lavfi", "-i", "color=black:s=256x256:d=0.2", "-c:v", enc}
	if enc == "h264_mf" {
		args = append(args, "-vf", "format=nv12", "-hw_encoding", "1") // no software fallback
	}
	out, err := noWindow(exec.CommandContext(ctx, ffmpeg, append(args, "-f", "null", "-")...)).CombinedOutput()
	if err == nil {
		return "ok"
	}
	if msg := ffmpegError(string(out)); msg != "" {
		return msg
	}
	return err.Error()
}

var (
	// ffmpeg's follow-up lines once an encoder has failed to open
	ffNoiseRe = regexp.MustCompile(`Error while opening encoder|Error applying encoder options|Could not open encoder|Error sending frames|Task finished with error|Terminating thread|Nothing was written|Conversion failed|Error opening output`)
	// "[vost#0:0/h264_mf @ 000001e9] [enc:h264_mf @ 000001e9] "
	ffTagRe = regexp.MustCompile(`^(?:\[[^\]]*\]\s*)+`)
)

// ffmpegError keeps the lines that say why an encoder failed ("The minimum
// required Nvidia driver for nvenc is 610.00 or newer"); they come before
// ffmpeg's generic "Error while opening encoder ... Nothing was written".
func ffmpegError(out string) string {
	var msgs []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(ffTagRe.ReplaceAllString(strings.TrimSpace(l), ""))
		if l == "" {
			continue
		}
		if ffNoiseRe.MatchString(l) {
			if len(msgs) == 0 {
				msgs = append(msgs, l)
			}
			break
		}
		if msgs = append(msgs, l); len(msgs) == 3 {
			break
		}
	}
	return strings.Join(msgs, " / ")
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
