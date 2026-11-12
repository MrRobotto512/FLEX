package server

// NVIDIA RTX Video Super Resolution as an upscaler. The driver offers it on
// the Direct3D 11 video processor (the switch Chrome/Edge and mpv use), so
// no SDK is needed, but only on a GeForce RTX card that drives a display,
// only when enlarging, and for one video at a time on the whole PC (seen
// on Robot: a second flexvsr gets the plain scaler while one runs). FFmpeg has no filter for it, so an upscaled stream
// becomes three programs in a row: FFmpeg decodes (and deinterlaces) to raw
// NV12 frames, flexvsr (tools/vsr, built into server/vsr) enlarges them on
// the card, and a second FFmpeg encodes them with the audio, read from the
// file again, into the usual HLS. At start FLEX installs flexvsr and checks
// on each NVIDIA card that the switch really changes the picture.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// vsrHelper is flexvsr on the card where super resolution engaged.
type vsrHelper struct {
	exe     string
	adapter int    // DXGI adapter index (flexvsr --adapter)
	card    string // its name, for the log and /server
}

func (t *Transcoder) setVSR(h *vsrHelper) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.vsr = h
}

// VSRReady says whether RTX Video Super Resolution passed its test here.
func (t *Transcoder) VSRReady() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.vsr != nil
}

// vsrFor is the helper this request goes through, or nil. SDR only (HDR
// goes the tone-mapping way), not with burned-in subtitles (the subtitles
// filter needs the file's own timing), not on the retries after a failure,
// and not during the pause after a stream it didn't engage in. It takes
// t.mu: call it before Start locks.
func (t *Transcoder) vsrFor(r transcodeRequest) *vsrHelper {
	if !r.Upscale || !r.VSR || r.CopyVid || r.NoGPU || r.Media.Video.HDR != "" || r.Sub >= 0 ||
		r.Media.Video.Width <= 0 || r.Media.Video.Height <= 0 {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// the driver enhances one video at a time on the whole PC: a stream of
	// ours already has it, so this one goes straight to the shader
	if t.vsr == nil || t.vsrBusy > 0 || time.Now().Before(t.vsrPause) {
		return nil
	}
	h := *t.vsr
	return &h
}

// vsrUpscale: this request goes through flexvsr.
func (t *Transcoder) vsrUpscale(r transcodeRequest) bool { return t.vsrFor(r) != nil }

// upscaleSize is the picture an upscaled stream has: the source's shape,
// as large as fits a 16:9 screen of the profile's height (1920x1080 for the
// PS5), even sizes. Filling the height alone made wide pictures wider than
// the screen (a 698x392 DVD rip: 1922x1080, 2556 for a 2.39:1 film), which
// level 4.1 doesn't allow: NVENC refused them and the stream fell back to
// Lanczos on the CPU.
func upscaleSize(w, h, maxH int) (int, int) {
	maxW := maxH * 16 / 9 &^ 1
	ow, oh := w*maxH/h, maxH
	if ow > maxW {
		ow, oh = maxW, h*maxW/w
	}
	return ow &^ 1, oh &^ 1
}

// frameRate is the source's frame rate for FFmpeg's -r, exact for the
// usual NTSC rates.
func frameRate(fps float64) string {
	for _, r := range []struct {
		num, den int
	}{{24000, 1001}, {30000, 1001}, {60000, 1001}} {
		if math.Abs(fps-float64(r.num)/float64(r.den)) < 0.01 {
			return fmt.Sprintf("%d/%d", r.num, r.den)
		}
	}
	if fps <= 0 || fps > 240 {
		return "24"
	}
	return strconv.FormatFloat(fps, 'f', -1, 64)
}

// vsrCommands are the three programs of an upscaled stream into dir:
// decode | flexvsr (h) | encode. The caller connects the pipes. It takes no
// lock (Start holds t.mu).
func (t *Transcoder) vsrCommands(ctx context.Context, r transcodeRequest, dir string, h vsrHelper) (dec, vsr, enc *exec.Cmd) {
	v := r.Media.Video
	iw, ih := v.Width&^1, v.Height&^1
	ow, oh := upscaleSize(v.Width, v.Height, r.Profile.MaxHeight)
	rate := frameRate(v.FPS)

	d := []string{"-hide_banner", "-loglevel", "warning", "-nostdin", "-hwaccel", "auto",
		"-readrate", "1.5", "-readrate_initial_burst", "60"}
	if r.Start > 0 {
		d = append(d, "-ss", fmt.Sprintf("%.3f", r.Start))
	}
	d = append(d, "-i", r.Path, "-map", fmt.Sprintf("0:%d", v.Index),
		"-vf", fmt.Sprintf("bwdif=mode=send_frame:deint=interlaced,scale=%d:%d,format=nv12", iw, ih),
		"-fps_mode", "cfr", "-r", rate, "-f", "rawvideo", "pipe:1")

	e := []string{"-hide_banner", "-loglevel", "warning", "-nostdin",
		"-thread_queue_size", "64", "-f", "rawvideo", "-pix_fmt", "nv12", "-s", fmt.Sprintf("%dx%d", ow, oh),
		"-r", rate, "-i", "pipe:0"}
	if r.Start > 0 {
		e = append(e, "-ss", fmt.Sprintf("%.3f", r.Start))
	}
	e = append(e, "-i", r.Path, "-map", "0:v")
	if r.Audio >= 0 {
		e = append(e, "-map", fmt.Sprintf("1:%d", r.Audio))
	} else {
		e = append(e, "-map", "1:a:0?")
	}
	if t.pixFormat(r) != "nv12" && t.encoderFor(r) != "h264_nvenc" {
		e = append(e, "-pix_fmt", "yuv420p") // libx264 (the CPU retry) doesn't take NV12
	}
	e = append(e, t.videoCodecArgs(r)...)
	e = append(e, "-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709", "-color_range", "tv")
	e = append(e, t.audioAndHLS(r, dir)...)

	dec = noWindow(exec.CommandContext(ctx, t.ffmpeg, d...))
	vsr = noWindow(exec.CommandContext(ctx, h.exe, strconv.Itoa(iw), strconv.Itoa(ih), strconv.Itoa(ow),
		strconv.Itoa(oh), "--adapter", strconv.Itoa(h.adapter)))
	enc = noWindow(exec.CommandContext(ctx, t.ffmpeg, e...))
	return dec, vsr, enc
}

// vsrMinSpeed: flexvsr alone must run this many times faster than real time
// (as aiMinSpeed: the stream is fed at 1.5x and still has to be encoded).
const vsrMinSpeed = 2.0

// vsrPauseFor: after a stream where super resolution didn't engage because
// of something outside FLEX (the driver enhances one video at a time on the
// whole PC, e.g. a video playing in Chrome; or the display is off), the
// other upscaler is used this long before trying again (each try costs the
// stream about a second).
const vsrPauseFor = 3 * time.Minute

var vsrFPS = regexp.MustCompile(`frames in [0-9.]+ s \(([0-9]+) fps\)`)

// prepareVSR installs flexvsr and finds the RTX card on which RTX Video
// Super Resolution changes the picture (the one driving a display, with the
// option on in the NVIDIA settings). The driver can stop and start applying
// it (seen on Robot within minutes, with nothing in the logs), so a card
// that takes the switch without effect right now is still used: each stream
// checks its first frame and falls back by itself. It reports under
// "rtxvsr"; without Direct3D (not Windows) it stays silent.
func (s *Server) prepareVSR(ctx context.Context, results map[string]string) {
	exe, err := installVSR(s.cfg.DataDir)
	if exe == "" && err == nil {
		return // not Windows
	}
	if err != nil {
		results["rtxvsr"] = "install failed: " + err.Error()
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	out, err := noWindow(exec.CommandContext(ctx, exe, "--list")).Output()
	if err != nil {
		results["rtxvsr"] = "flexvsr: " + err.Error()
		return
	}
	cards := strings.TrimSpace(string(out))
	if cards == "" {
		results["rtxvsr"] = "no NVIDIA graphics card"
		return
	}
	frames, err := vsrTestFrames(ctx, s.cfg.FFmpeg)
	if err != nil {
		results["rtxvsr"] = "test picture: " + err.Error()
		return
	}
	var accepted *vsrHelper // the first RTX card that took the switch without effect
	for _, line := range strings.Split(cards, "\n") {
		idx, name, _ := strings.Cut(strings.TrimSpace(line), "\t")
		adapter, err := strconv.Atoi(idx)
		if err != nil || !strings.Contains(name, "RTX") {
			continue // GTX and older cards don't have it
		}
		short := strings.TrimPrefix(name, "NVIDIA ")
		fps, err := runVSRTest(ctx, exe, adapter, frames)
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 4 && !strings.Contains(err.Error(), "not available") {
			s.log.Info("RTX Video Super Resolution takes no effect right now", "card", name)
			if accepted == nil {
				accepted = &vsrHelper{exe: exe, adapter: adapter, card: name}
			}
			continue
		}
		if err != nil {
			s.log.Info("RTX Video Super Resolution test", "card", name, "err", err)
			continue
		}
		if speed := fps / 24; speed < vsrMinSpeed {
			results["rtxvsr"] = fmt.Sprintf("too slow on the %s (%.1fx real time)", short, speed)
		} else {
			results["rtxvsr"] = fmt.Sprintf("ok (%.0fx real time on the %s)", speed, short)
			s.tc.setVSR(&vsrHelper{exe: exe, adapter: adapter, card: name})
		}
		return
	}
	if accepted != nil {
		results["rtxvsr"] = fmt.Sprintf("ok, but not engaging right now on the %s (each stream checks; it needs "+
			"the display on and RTX Video super resolution on in the NVIDIA settings)", strings.TrimPrefix(accepted.card, "NVIDIA "))
		s.tc.setVSR(accepted)
		return
	}
	results["rtxvsr"] = "no GeForce RTX card that takes it"
}

// vsrTestFrames is two seconds of FFmpeg's moving test picture at 640x360,
// as raw NV12.
func vsrTestFrames(ctx context.Context, ffmpeg string) ([]byte, error) {
	out, err := noWindow(exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "lavfi", "-i", "testsrc2=s=640x360:r=24:d=2", "-pix_fmt", "nv12", "-f", "rawvideo", "pipe:1")).Output()
	if err != nil {
		return nil, err
	}
	if len(out) != 640*360*3/2*48 {
		return nil, fmt.Errorf("got %d bytes", len(out))
	}
	return out, nil
}

// runVSRTest enlarges the test frames to 1080p on adapter and returns
// flexvsr's speed. flexvsr checks the effect on the first frame itself: exit
// code 4 when the driver refused the switch or it changed nothing.
func runVSRTest(ctx context.Context, exe string, adapter int, frames []byte) (float64, error) {
	cmd := noWindow(exec.CommandContext(ctx, exe, "640", "360", "1920", "1080", "--adapter", strconv.Itoa(adapter)))
	cmd.Stdin = bytes.NewReader(frames)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	fps := 0.0
	if m := vsrFPS.FindStringSubmatch(stderr.String()); m != nil {
		fps, _ = strconv.ParseFloat(m[1], 64)
	}
	return fps, nil
}

// logLines copies a program's messages into the log: flexvsr's few lines
// (which card, whether the switch took) as info, ffmpeg's as debug.
func (t *Transcoder) logLines(key, who string, r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if who == "flexvsr" {
			t.log.Info(who, "key", key, "line", sc.Text())
		} else {
			t.log.Debug(who, "key", key, "line", sc.Text())
		}
	}
}
