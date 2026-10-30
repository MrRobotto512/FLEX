package server

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// fakeBuild is a zip laid out like gyan.dev's: <name>/bin/ffmpeg.exe etc.
func fakeBuild(t *testing.T) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		"ffmpeg-x-essentials_build/bin/ffmpeg.exe":  "ffmpeg binary",
		"ffmpeg-x-essentials_build/bin/ffprobe.exe": "ffprobe binary",
		"ffmpeg-x-essentials_build/bin/ffplay.exe":  "not needed",
		"ffmpeg-x-essentials_build/README.txt":      "readme",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDownloadAndUnpack(t *testing.T) {
	build := fakeBuild(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(build) }))
	defer srv.Close()
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "download.zip")

	if err := downloadFile(context.Background(), srv.URL, zipPath, sha([]byte("something else")), nil); err == nil {
		t.Fatal("a wrong checksum was accepted")
	}
	var calls int
	if err := downloadFile(context.Background(), srv.URL, zipPath, sha(build), func(done, total int64) { calls++ }); err != nil {
		t.Fatal(err)
	}
	if calls == 0 {
		t.Error("progress was never reported")
	}

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, name := range []string{"ffmpeg.exe", "ffprobe.exe"} {
		if err := unzipBin(&zr.Reader, name, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "ffprobe.exe")); string(b) != "ffprobe binary" {
		t.Fatalf("ffprobe.exe = %q", b)
	}
	if err := unzipBin(&zr.Reader, "ffmpeg-missing.exe", filepath.Join(dir, "x")); err == nil {
		t.Error("a missing entry was not reported")
	}
}

func TestFindToolUsesDownloadedCopy(t *testing.T) {
	data := t.TempDir()
	if got := findTool("ffmpeg", data); got != "ffmpeg" {
		t.Fatalf("nothing downloaded: got %q; want the bare name for PATH", got)
	}
	dir := ffmpegDir(data)
	os.MkdirAll(dir, 0o755)
	want := filepath.Join(dir, exeName("ffmpeg"))
	os.WriteFile(want, []byte("x"), 0o755)
	if got := findTool("ffmpeg", data); got != want {
		t.Fatalf("got %q; want %q", got, want)
	}
}
