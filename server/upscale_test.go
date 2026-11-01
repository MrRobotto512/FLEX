package server

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAIUpscaleArgs(t *testing.T) {
	sd := &MediaInfo{Video: VideoInfo{Codec: "mpeg4", Width: 640, Height: 480}}
	r := transcodeRequest{Path: "futurama.avi", Media: sd, Profile: profiles["ps5"], Audio: -1, Sub: -1, Upscale: true,
		Shader: `C:\FLEX\data\shaders\anime4k.glsl`}
	vf := func(args []string) string {
		for i, a := range args {
			if a == "-vf" {
				return args[i+1]
			}
		}
		return ""
	}
	has := strings.Contains

	// libplacebo works: deinterlace, then the shader up to 1440x1080 in BT.709, NVENC
	tc := &Transcoder{encoder: "h264_nvenc", hdrGPU: true}
	args := tc.args(r, t.TempDir())
	f := vf(args)
	if !contains(args, "vulkan=vk") || !has(f, "bwdif=") || !has(f, "libplacebo=w=1440:h=1080") ||
		!has(f, `custom_shader_path='C\:/FLEX/data/shaders/anime4k.glsl'`) || !has(f, "colorspace=bt709") ||
		has(f, "scale_cuda") || !contains(args, "h264_nvenc") {
		t.Errorf("AI upscale: %q", args)
	}
	// Media Foundation takes nv12
	if f = vf((&Transcoder{encoder: "h264_mf", hdrGPU: true}).args(r, t.TempDir())); !has(f, "format=nv12") {
		t.Errorf("AI upscale via Media Foundation: %s", f)
	}
	// no shader (remote, or this GPU too slow): Lanczos on CUDA as before
	r.Shader = ""
	if f = vf(tc.args(r, t.TempDir())); has(f, "libplacebo") || !has(f, "interp_algo=lanczos") {
		t.Errorf("Lanczos upscale: %s", f)
	}
	// the retry after the GPU pipeline failed: Lanczos on the CPU
	r.Shader, r.NoGPU = `C:\x.glsl`, true
	if f = vf(tc.args(r, t.TempDir())); has(f, "libplacebo") || !has(f, "flags=lanczos") {
		t.Errorf("CPU retry: %s", f)
	}
	// a different shader is a different stream
	a := transcodeRequest{Path: "x", Upscale: true, Shader: "a"}
	b := a
	b.Shader = "b"
	if a.Key() == b.Key() {
		t.Error("the shader must be part of the session key")
	}
}

func TestShaderTestPicksShaders(t *testing.T) {
	tc := &Transcoder{}
	if tc.Shader(true) != "" || tc.Shader(false) != "" {
		t.Error("no shaders before their test")
	}
	tc.setShader("anime4k", "a.glsl")
	tc.setShader("fsrcnnx", "f.glsl")
	if tc.Shader(true) != "a.glsl" || tc.Shader(false) != "f.glsl" {
		t.Errorf("animation %q, rest %q", tc.Shader(true), tc.Shader(false))
	}
}

// A shader comes from its release once: a zip's shaders joined in order, a
// plain file as it is, a wrong checksum refused.
func TestEnsureShader(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "a.zip")
	zf, _ := os.Create(zipPath)
	zw := zip.NewWriter(zf)
	for name, text := range map[string]string{"pack/B.glsl": "//!HOOK B", "pack/A.glsl": "//!HOOK A", "pack/C.glsl": "//!HOOK C"} {
		w, _ := zw.Create(name)
		w.Write([]byte(text))
	}
	zw.Close()
	zf.Close()
	zipBytes, _ := os.ReadFile(zipPath)
	plain := []byte("//!HOOK LUMA")
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/a.zip" {
			w.Write(zipBytes)
		} else {
			w.Write(plain)
		}
	}))
	defer srv.Close()
	sum := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	dir := t.TempDir()
	ctx := context.Background()

	file, err := ensureShader(ctx, dir, aiShader{name: "z", url: srv.URL + "/a.zip", sha256: sum(zipBytes), files: []string{"A.glsl", "C.glsl"}})
	if b, _ := os.ReadFile(file); err != nil || string(b) != "//!HOOK A\n//!HOOK C\n" {
		t.Fatalf("from a zip: %q, %v", b, err)
	}
	if _, err := ensureShader(ctx, dir, aiShader{name: "z", url: srv.URL + "/a.zip", sha256: "x", files: []string{"A.glsl"}}); err != nil || hits != 1 {
		t.Errorf("a shader already there must not be fetched again: %v, %d fetches", err, hits)
	}
	file, err = ensureShader(ctx, dir, aiShader{name: "p", url: srv.URL + "/p.glsl", sha256: sum(plain)})
	if b, _ := os.ReadFile(file); err != nil || string(b) != string(plain) {
		t.Errorf("plain: %q, %v", b, err)
	}
	if _, err := ensureShader(ctx, dir, aiShader{name: "bad", url: srv.URL + "/p.glsl", sha256: sum([]byte("other"))}); err == nil {
		t.Error("a wrong checksum must be refused")
	}
	if _, err := os.Stat(filepath.Join(dir, "bad.glsl")); err == nil {
		t.Error("a refused download must leave no shader behind")
	}
	if _, err := ensureShader(ctx, dir, aiShader{name: "m", url: srv.URL + "/a.zip", sha256: sum(zipBytes), files: []string{"D.glsl"}}); err == nil {
		t.Error("a shader missing from the zip must be an error")
	}
}

// The pinned releases still download, match their checksums and pass
// through this PC's FFmpeg (needs the network and FFmpeg with libplacebo).
func TestAIShadersForReal(t *testing.T) {
	if testing.Short() || os.Getenv("FLEX_TEST_SHADERS") == "" {
		t.Skip("set FLEX_TEST_SHADERS=1 (network, FFmpeg with libplacebo and Vulkan)")
	}
	dir := t.TempDir()
	for _, sh := range aiShaders {
		file, err := ensureShader(context.Background(), dir, sh)
		if err != nil {
			t.Fatalf("%s: %v", sh.name, err)
		}
		start := time.Now()
		speed, err := ShaderTest(context.Background(), "ffmpeg", file)
		t.Logf("%s: %.1fx real time (%v), %v", sh.name, speed, time.Since(start).Round(time.Millisecond), err)
		if err != nil || speed <= 0 {
			t.Errorf("%s: %v", sh.name, err)
		}
	}
}
