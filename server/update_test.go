package server

import (
	"archive/zip"
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewerVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.3.0", "0.2.0", true},
		{"0.2.0", "0.3.0", false},
		{"0.3.0", "0.3.0", false},
		{"0.10.0", "0.9.9", true},
		{"1.0", "0.9.9", true},
		{"v0.3.1", "0.3.0", true},
		{"0.3.0", "5f165c9-dirty", true}, // dev build: any release is newer
		{"5f165c9", "0.3.0", false},      // a dev build is never offered
		{"", "0.3.0", false},
	}
	for _, c := range cases {
		if got := NewerVersion(c.a, c.b); got != c.want {
			t.Errorf("NewerVersion(%q, %q) = %v; want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestPS5ClientStore(t *testing.T) {
	s := &Server{cfg: Config{DataDir: t.TempDir()}, log: slog.Default()}
	s.ps5up = &ps5Updates{s: s}

	rec := httptest.NewRecorder()
	s.handlePS5Client(rec, httptest.NewRequest(http.MethodGet, "/api/v1/client/ps5", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("no build: HTTP %d; want 404", rec.Code)
	}

	elf := []byte("\x7fELF test build")
	if err := s.StorePS5Client("0.3.0", elf, nil); err != nil {
		t.Fatal(err)
	}
	// a restart finds it again
	s.ps5up = &ps5Updates{s: s}
	s.ps5up.load()
	cl := s.ps5up.current()
	if cl == nil || cl.Version != "0.3.0" || cl.Size != len(elf) || cl.SHA256 != sha(elf) {
		t.Fatalf("after reload: %+v", cl)
	}

	rec = httptest.NewRecorder()
	s.handlePS5ClientFile(rec, httptest.NewRequest(http.MethodGet, cl.URL, nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Body.String(), "\x7fELF") {
		t.Fatalf("file: HTTP %d %q", rec.Code, rec.Body.String())
	}
}

func TestPS5AppStore(t *testing.T) {
	s := &Server{cfg: Config{DataDir: t.TempDir()}, log: slog.Default()}
	s.ps5up = &ps5Updates{s: s}

	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	for name, body := range map[string]string{
		"INSTALL.txt":                   "copy PPSA84840 to /data/homebrew",
		"PPSA84840/eboot.bin":           "SELF app",
		"PPSA84840/sce_module/libc.prx": "prx",
		"PPSA84840/sce_sys/param.json":  "{}",
	} {
		w, _ := zw.Create(name)
		w.Write([]byte(body))
	}
	zw.Close()

	if err := s.StorePS5Client("0.8.0", []byte("\x7fELF"), zb.Bytes()); err != nil {
		t.Fatal(err)
	}
	s.ps5up = &ps5Updates{s: s}
	s.ps5up.load()
	cl := s.ps5up.current()
	if cl == nil || cl.App == nil || cl.App.TitleID != "PPSA84840" || len(cl.App.Files) != 3 {
		t.Fatalf("after reload: %+v", cl)
	}
	if f := cl.App.Files[0]; f.Path != "eboot.bin" || f.URL != "/api/v1/client/ps5/app/eboot.bin" || f.SHA256 != sha([]byte("SELF app")) {
		t.Fatalf("first file: %+v", f)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/client/ps5/app/{path...}", s.handlePS5AppFile)
	for path, served := range map[string]bool{"sce_module/libc.prx": true, "INSTALL.txt": false, "../client.json": false} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/client/ps5/app/"+path, nil))
		if (rec.Code == http.StatusOK) != served {
			t.Errorf("%s: HTTP %d; served should be %v", path, rec.Code, served)
		}
	}

	// a zip without an app folder is refused
	var bad bytes.Buffer
	zw = zip.NewWriter(&bad)
	w, _ := zw.Create("eboot.bin")
	w.Write([]byte("x"))
	zw.Close()
	if err := s.StorePS5Client("0.8.1", []byte("\x7fELF"), bad.Bytes()); err == nil {
		t.Error("zip without <TITLEID>/eboot.bin was accepted")
	}
}
