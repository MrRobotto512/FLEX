package server

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"time"
)

// AI upscaling: when a client on the home network asks for upscaling, a
// libplacebo pass runs a small neural network on the GPU in place of plain
// Lanczos: Anime4K for animation, FSRCNNX for everything else. The shaders
// aren't part of FLEX: they come from their own projects' GitHub releases on
// the first start (checksums pinned), and a short speed test decides whether
// this PC's GPU is quick enough; if not, upscaling stays Lanczos.

type aiShader struct {
	name   string // also the file name in DataDir/shaders
	label  string
	url    string
	sha256 string
	files  []string // the shaders to take from a zip, in order; nil: the download is the shader
}

var aiShaders = []aiShader{
	{"anime4k", "Anime4K", "https://github.com/bloc97/Anime4K/releases/download/v4.0.1/Anime4K_v4.0.zip",
		"139cd282086457c5adc79caf7b75b8b825091d71c9b54958c18745fea62d7ed7",
		[]string{"Anime4K_Clamp_Highlights.glsl", "Anime4K_Restore_CNN_M.glsl", "Anime4K_Upscale_CNN_x2_M.glsl"}},
	{"fsrcnnx", "FSRCNNX", "https://github.com/igv/FSRCNN-TensorFlow/releases/download/1.1/FSRCNNX_x2_8-0-4-1.glsl",
		"e800dbc5c1c95185cc82216c597724533ff5f2880179f256eef600f03e8dc2ae", nil},
}

// aiMinSpeed: the shader pass alone must run this many times faster than
// real time (a transcode is fed at 1.5x and still has to be encoded).
const aiMinSpeed = 2.0

// prepareUpscaling fetches and tests the shaders in the background at start.
func (s *Server) prepareUpscaling(ctx context.Context) {
	results := map[string]string{}
	defer func() {
		s.mu.Lock()
		s.upscalers = results
		s.mu.Unlock()
		s.log.Info("AI upscaling", "shaders", results)
	}()
	if !s.tc.hdrGPU {
		results["ai"] = "needs libplacebo (Vulkan), which didn't pass its test"
		return
	}
	dir := filepath.Join(s.cfg.DataDir, "shaders")
	for _, sh := range aiShaders {
		file, err := ensureShader(ctx, dir, sh)
		if err != nil {
			results[sh.name] = "download failed: " + err.Error()
			continue
		}
		speed, err := ShaderTest(ctx, s.cfg.FFmpeg, file)
		switch {
		case err != nil:
			results[sh.name] = err.Error()
		case speed < aiMinSpeed:
			results[sh.name] = fmt.Sprintf("too slow on this GPU (%.1fx real time)", speed)
		default:
			results[sh.name] = fmt.Sprintf("ok (%.1fx real time)", speed)
			s.tc.setShader(sh.name, file)
		}
	}
}

// ensureShader returns the shader's file, downloading it first if needed.
func ensureShader(ctx context.Context, dir string, sh aiShader) (string, error) {
	file := filepath.Join(dir, sh.name+".glsl")
	if fi, err := os.Stat(file); err == nil && fi.Size() > 0 {
		return file, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dl := filepath.Join(dir, sh.name+".download")
	defer os.Remove(dl)
	if err := downloadFile(ctx, sh.url, dl, sh.sha256, nil); err != nil {
		return "", err
	}
	var text []byte
	if sh.files == nil {
		b, err := os.ReadFile(dl)
		if err != nil {
			return "", err
		}
		text = b
	} else {
		b, err := shadersFromZip(dl, sh.files)
		if err != nil {
			return "", err
		}
		text = b
	}
	tmp := file + ".part"
	if err := os.WriteFile(tmp, text, 0o644); err != nil {
		return "", err
	}
	return file, os.Rename(tmp, file)
}

// shadersFromZip joins the named shaders from a release zip into one file
// (libplacebo takes one; mpv's .hook format allows many passes in it).
func shadersFromZip(zipPath string, names []string) ([]byte, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var out bytes.Buffer
	for _, name := range names {
		found := false
		for _, f := range zr.File {
			if path.Base(f.Name) != name {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			_, err = io.Copy(&out, io.LimitReader(rc, 4<<20))
			rc.Close()
			if err != nil {
				return nil, err
			}
			out.WriteString("\n")
			found = true
			break
		}
		if !found {
			return nil, fmt.Errorf("%s isn't in the download", name)
		}
	}
	return out.Bytes(), nil
}

// aiFilter upscales to w×h through the shader and EWA Lanczos for the last
// stretch, into BT.709 (an HD picture) in the layout the encoder takes.
func aiFilter(shader string, w, h int, format string) string {
	return fmt.Sprintf("libplacebo=w=%d:h=%d:upscaler=ewa_lanczos:custom_shader_path=%s:"+
		"colorspace=bt709:color_primaries=bt709:color_trc=bt709:range=tv:format=%s", w, h, ffEscape(shader), format)
}

// ShaderTest runs a 640x480 test picture through the shader up to 1440x1080
// and says how many times faster than real time it goes. One second and six
// seconds of it are timed, so FFmpeg's start (Vulkan, compiling the shader)
// cancels out of the difference.
func ShaderTest(ctx context.Context, ffmpeg, shader string) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	run := func(secs int) (time.Duration, error) {
		args := []string{"-hide_banner", "-loglevel", "error", "-nostdin",
			"-init_hw_device", "vulkan=vk", "-filter_hw_device", "vk",
			"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=s=640x480:r=30:d=%d", secs),
			"-vf", aiFilter(shader, 1440, 1080, "yuv420p"), "-f", "null", "-"}
		start := time.Now()
		out, err := noWindow(exec.CommandContext(ctx, ffmpeg, args...)).CombinedOutput()
		if err != nil {
			if msg := ffmpegError(string(out)); msg != "" {
				return 0, errors.New(msg)
			}
			return 0, err
		}
		return time.Since(start), nil
	}
	short, err := run(1)
	if err != nil {
		return 0, err
	}
	long, err := run(6)
	if err != nil {
		return 0, err
	}
	if d := long - short; d > 0 {
		return min(5/d.Seconds(), 99), nil
	}
	return 99, nil
}

// Upscalers says how each AI upscaler fared at start ("ok (9.5x real time)",
// "too slow …", why not), for /server and the desktop window.
func (s *Server) Upscalers() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upscalers
}
