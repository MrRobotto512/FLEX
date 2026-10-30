package server

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// FFmpeg is looked for next to the executable (portable installs), then in
// the data dir, then on PATH. On Windows, when none has it, FLEX downloads
// gyan.dev's "essentials" build (pinned version, SHA-256 checked) into
// <data>/ffmpeg/<version>, so a new PC needs nothing but FLEX.exe. That build
// has every encoder and filter FLEX uses: NVENC, QuickSync (libvpl), AMF,
// Media Foundation, x264, scale_cuda, zscale and libass.

const (
	ffmpegVersion = "9.0.2"
	ffmpegURL     = "https://github.com/GyanD/codexffmpeg/releases/download/" + ffmpegVersion +
		"/ffmpeg-" + ffmpegVersion + "-essentials_build.zip"
	ffmpegSHA256 = "60f467265b1e312373dbcd92200c2618a74850f98d3d078e94296bb3fa2047ba"
	ffmpegMaxZip = 512 << 20
)

// CanDownloadFFmpeg reports whether EnsureFFmpeg has a build for this OS.
func CanDownloadFFmpeg() bool { return runtime.GOOS == "windows" }

func ffmpegDir(dataDir string) string { return filepath.Join(dataDir, "ffmpeg", ffmpegVersion) }

func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// findTool returns ffmpeg/ffprobe next to this executable, else the copy
// EnsureFFmpeg downloaded, else the bare name for a PATH lookup.
func findTool(name, dataDir string) string {
	candidates := []string{}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), exeName(name)))
	}
	if dataDir != "" {
		candidates = append(candidates, filepath.Join(ffmpegDir(dataDir), exeName(name)))
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return name
}

// FFmpegAvailable reports whether the configured ffmpeg and ffprobe exist
// (call after Validate, which resolves them).
func (c Config) FFmpegAvailable() bool {
	for _, p := range []string{c.FFmpeg, c.FFprobe} {
		if _, err := exec.LookPath(p); err != nil {
			return false
		}
	}
	return true
}

// EnsureFFmpeg downloads and unpacks the pinned FFmpeg build into the data
// dir unless it is already there. progress, if set, is called as the
// download goes (total is the zip's size, 0 if unknown).
func EnsureFFmpeg(ctx context.Context, dataDir string, progress func(done, total int64)) error {
	if !CanDownloadFFmpeg() {
		return fmt.Errorf("no FFmpeg download for %s; install ffmpeg and ffprobe", runtime.GOOS)
	}
	dir := ffmpegDir(dataDir)
	if haveFFmpeg(dir) {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	zipPath := filepath.Join(dir, "download.zip")
	defer os.Remove(zipPath)
	if err := downloadFile(ctx, ffmpegURL, zipPath, ffmpegSHA256, progress); err != nil {
		return fmt.Errorf("download FFmpeg: %w", err)
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		if err := unzipBin(&zr.Reader, exeName(name), filepath.Join(dir, exeName(name))); err != nil {
			return err
		}
	}
	return nil
}

func haveFFmpeg(dir string) bool {
	for _, n := range []string{"ffmpeg", "ffprobe"} {
		if _, err := os.Stat(filepath.Join(dir, exeName(n))); err != nil {
			return false
		}
	}
	return true
}

// unzipBin extracts the bin/<name> entry of the build's zip to dst (via a
// temporary name, so an interrupted extraction never leaves a broken exe).
func unzipBin(zr *zip.Reader, name, dst string) error {
	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, "/bin/"+name) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		tmp := dst + ".part"
		out, err := os.Create(tmp)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, rc)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(tmp)
			return fmt.Errorf("unpack %s: %w", name, err)
		}
		return os.Rename(tmp, dst)
	}
	return fmt.Errorf("the FFmpeg download has no bin/%s", name)
}

// downloadFile streams url to dst, checking its SHA-256 on the way.
func downloadFile(ctx context.Context, url, dst, wantSHA string, progress func(done, total int64)) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "FLEX/"+Version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	h := sha256.New()
	pw := &progressWriter{total: resp.ContentLength, fn: progress}
	_, err = io.Copy(io.MultiWriter(out, h, pw), io.LimitReader(resp.Body, ffmpegMaxZip))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA {
		return fmt.Errorf("checksum mismatch (%s)", got)
	}
	return nil
}

type progressWriter struct {
	done, total int64
	last        time.Time
	fn          func(done, total int64)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if p.fn != nil && (time.Since(p.last) > 250*time.Millisecond || p.done == p.total) {
		p.last = time.Now()
		p.fn(p.done, p.total)
	}
	return len(b), nil
}
