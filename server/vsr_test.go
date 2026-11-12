package server

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Start must not wait on its own lock when it builds the super-resolution
// pipeline (it did: the session never started and the request hung). With
// programs that don't exist it has to come back with an error, quickly.
func TestVSRStartReturns(t *testing.T) {
	tc := NewTranscoder(filepath.Join(t.TempDir(), "no-ffmpeg"), "h264_nvenc", t.TempDir(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	tc.setVSR(&vsrHelper{exe: filepath.Join(t.TempDir(), "no-flexvsr.exe")})
	r := transcodeRequest{Path: "x.mkv", Media: &MediaInfo{Video: VideoInfo{Width: 720, Height: 480, FPS: 29.97}},
		Profile: profiles["ps5"], Audio: -1, Sub: -1, Upscale: true, VSR: true}
	done := make(chan error, 1)
	go func() {
		_, err := tc.Start(context.Background(), r)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("programs that don't exist can't make a stream")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Start hung")
	}
}

func TestUpscaleSizeAndFrameRate(t *testing.T) {
	for _, c := range [][4]int{{698, 392, 1920, 1078}, {720, 304, 1920, 810}, {720, 480, 1620, 1080},
		{640, 480, 1440, 1080}, {1280, 720, 1920, 1080}} {
		if w, h := upscaleSize(c[0], c[1], 1080); w != c[2] || h != c[3] {
			t.Errorf("upscaleSize(%dx%d) = %dx%d; want %dx%d (inside 1920x1080)", c[0], c[1], w, h, c[2], c[3])
		}
	}
	if w, h := upscaleSize(720, 304, 2160); w != 3840 || h != 1620 {
		t.Errorf("4K profile: %dx%d", w, h)
	}
	for in, want := range map[float64]string{23.976: "24000/1001", 29.97: "30000/1001", 59.94: "60000/1001",
		25: "25", 24: "24", 0: "24", 12.5: "12.5"} {
		if got := frameRate(in); got != want {
			t.Errorf("frameRate(%v) = %s; want %s", in, got, want)
		}
	}
}

func TestVSRPipeline(t *testing.T) {
	sd := &MediaInfo{Video: VideoInfo{Index: 0, Codec: "h264", Width: 698, Height: 392, FPS: 23.976}}
	r := transcodeRequest{Path: "mickey.mkv", Media: sd, Profile: profiles["ps5"], Audio: -1, Sub: -1,
		Start: 900, Upscale: true, Shader: "fsrcnnx.glsl", VSR: true}
	tc := &Transcoder{ffmpeg: "ffmpeg", encoder: "h264_nvenc", hdrGPU: true,
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	if tc.vsrUpscale(r) {
		t.Error("no helper yet: no RTX Video Super Resolution")
	}
	tc.setVSR(&vsrHelper{exe: "flexvsr.exe", adapter: 0, card: "NVIDIA GeForce RTX 4070"})
	if !tc.vsrUpscale(r) {
		t.Fatal("helper ready, SD, upscaling: it should be used")
	}
	for name, mod := range map[string]func(*transcodeRequest){
		"not asked":         func(r *transcodeRequest) { r.VSR = false },
		"HDR":               func(r *transcodeRequest) { m := *r.Media; m.Video.HDR = "hdr10"; r.Media = &m },
		"burned subtitles":  func(r *transcodeRequest) { r.Sub = 3 },
		"retry on the CPU":  func(r *transcodeRequest) { r.NoGPU = true },
		"remux":             func(r *transcodeRequest) { r.CopyVid = true },
		"no upscale wanted": func(r *transcodeRequest) { r.Upscale = false },
	} {
		c := r
		mod(&c)
		if tc.vsrUpscale(c) {
			t.Errorf("%s: should not go through flexvsr", name)
		}
	}
	tc.vsrBusy = 1
	if tc.vsrUpscale(r) {
		t.Error("one of our streams has it already: the driver serves one video at a time")
	}
	tc.vsrBusy = 0
	plain := r
	plain.VSR = false
	if r.Key() == plain.Key() {
		t.Error("with and without super resolution must be different sessions")
	}

	h := tc.vsrFor(r)
	dec, up, enc := tc.vsrCommands(context.Background(), r, t.TempDir(), *h)
	d, u, e := strings.Join(dec.Args, " "), strings.Join(up.Args, " "), strings.Join(enc.Args, " ")
	for _, want := range []string{"-ss 900.000 -i mickey.mkv -map 0:0", "bwdif=mode=send_frame:deint=interlaced,scale=698:392,format=nv12",
		"-fps_mode cfr -r 24000/1001 -f rawvideo pipe:1", "-readrate 1.5"} {
		if !strings.Contains(d, want) {
			t.Errorf("decode lacks %q: %s", want, d)
		}
	}
	if !strings.HasSuffix(u, "698 392 1920 1078 --adapter 0") {
		t.Errorf("flexvsr: %s", u)
	}
	for _, want := range []string{"-f rawvideo -pix_fmt nv12 -s 1920x1078 -r 24000/1001 -i pipe:0", "-ss 900.000 -i mickey.mkv",
		"-map 0:v -map 1:a:0?", "-c:v h264_nvenc", "-b:v 8000000", "-colorspace bt709", "-f hls"} {
		if !strings.Contains(e, want) {
			t.Errorf("encode lacks %q: %s", want, e)
		}
	}
	if strings.Contains(e, "-readrate") || strings.Contains(e, "pix_fmt yuv420p") {
		t.Errorf("encode: the decoder paces the stream, and NVENC takes NV12: %s", e)
	}

	// a chosen audio track comes from the second input; libx264 gets yuv420p
	r.Audio, r.CPUOnly = 2, true
	_, _, enc = tc.vsrCommands(context.Background(), r, t.TempDir(), *h)
	if e := strings.Join(enc.Args, " "); !strings.Contains(e, "-map 1:2") || !strings.Contains(e, "-pix_fmt yuv420p") ||
		!strings.Contains(e, "libx264") {
		t.Errorf("audio track / CPU encoder: %s", e)
	}
}
