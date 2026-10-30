package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Updates come from the GitHub releases of the FLEX repository. CI attaches
// the Windows app, flexd and the PS5 client to every release, plus a
// SHA256SUMS file that every download is checked against.
//
// The PS5's own HTTP client has no TLS, so consoles don't talk to GitHub:
// the server keeps the newest PS5 client in its data dir and offers it at
// /api/v1/client/ps5, over the same connection the console already uses (at
// home or through Tailscale). The desktop app uses LatestRelease and
// Release.Download to replace itself.

const (
	ReleaseRepo  = "MrRobotto512/FLEX"
	PS5Asset     = "FLEX-ps5.elf"
	sumsAsset    = "SHA256SUMS"
	updateEvery  = 6 * time.Hour
	maxAssetSize = 256 << 20
)

// Release is one published FLEX release.
type Release struct {
	Version string            `json:"version"` // tag without the leading "v"
	URL     string            `json:"url"`     // release page
	Notes   string            `json:"notes,omitempty"`
	assets  map[string]string // file name -> download URL
	sums    map[string]string // file name -> sha256
}

// Has reports whether the release carries a file (with a checksum).
func (r *Release) Has(name string) bool { return r.assets[name] != "" && r.sums[name] != "" }

// LatestRelease asks GitHub for the newest release and its checksums.
func LatestRelease(ctx context.Context) (*Release, error) {
	var gh struct {
		Tag    string `json:"tag_name"`
		URL    string `json:"html_url"`
		Body   string `json:"body"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	body, err := fetch(ctx, "https://api.github.com/repos/"+ReleaseRepo+"/releases/latest", 1<<20)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(body, &gh); err != nil {
		return nil, fmt.Errorf("release: %w", err)
	}
	r := &Release{Version: strings.TrimPrefix(gh.Tag, "v"), URL: gh.URL, Notes: gh.Body,
		assets: map[string]string{}, sums: map[string]string{}}
	for _, a := range gh.Assets {
		r.assets[a.Name] = a.URL
	}
	if r.assets[sumsAsset] == "" {
		return nil, fmt.Errorf("release %s has no %s", gh.Tag, sumsAsset)
	}
	sums, err := fetch(ctx, r.assets[sumsAsset], 64<<10)
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(strings.NewReader(string(sums)))
	for sc.Scan() { // sha256sum format: "<hash>  <name>" (a "*" marks binary mode)
		if f := strings.Fields(sc.Text()); len(f) == 2 {
			r.sums[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
		}
	}
	return r, nil
}

// Download fetches one file of the release and checks it against SHA256SUMS.
func (r *Release) Download(ctx context.Context, name string) ([]byte, error) {
	if !r.Has(name) {
		return nil, fmt.Errorf("release %s has no %s", r.Version, name)
	}
	b, err := fetch(ctx, r.assets[name], maxAssetSize)
	if err != nil {
		return nil, err
	}
	if got := sha(b); got != r.sums[name] {
		return nil, fmt.Errorf("%s: checksum mismatch (%s)", name, got)
	}
	return b, nil
}

func fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "FLEX/"+Version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("GET %s: larger than %d bytes", url, limit)
	}
	return b, nil
}

// NewerVersion reports whether version a is newer than b. Versions are
// dotted numbers ("0.3.1"); anything else in b (a dev build such as
// "5f165c9-dirty") counts as older than any release, so it gets offered one.
func NewerVersion(a, b string) bool {
	pa, okA := parseVersion(a)
	pb, okB := parseVersion(b)
	if !okA {
		return false
	}
	if !okB {
		return true
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) == 0 || len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// ---------------------------------------------------------------- PS5 client

// PS5Client describes the console build the server offers.
type PS5Client struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	Size    int    `json:"size"`
	URL     string `json:"url"`
}

type ps5Updates struct {
	s  *Server
	mu sync.Mutex
	cl *PS5Client // nil until a build is stored
}

func (u *ps5Updates) dir() string { return filepath.Join(u.s.cfg.DataDir, "ps5", "client") }

// load picks up the build stored by an earlier run.
func (u *ps5Updates) load() {
	b, err := os.ReadFile(filepath.Join(u.dir(), "client.json"))
	if err != nil {
		return
	}
	var cl PS5Client
	if json.Unmarshal(b, &cl) != nil {
		return
	}
	if elf, err := os.ReadFile(filepath.Join(u.dir(), "eboot.elf")); err == nil && sha(elf) == cl.SHA256 {
		u.mu.Lock()
		u.cl = &cl
		u.mu.Unlock()
	}
}

func (u *ps5Updates) current() *PS5Client {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.cl
}

// store keeps elf as the offered build; the files are replaced atomically
// so a console downloading the old one still gets a whole file.
func (u *ps5Updates) store(version string, elf []byte) error {
	if err := os.MkdirAll(u.dir(), 0o755); err != nil {
		return err
	}
	cl := &PS5Client{Version: version, SHA256: sha(elf), Size: len(elf), URL: "/api/v1/client/ps5/eboot.elf"}
	meta, _ := json.MarshalIndent(cl, "", "  ")
	for name, data := range map[string][]byte{"eboot.elf": elf, "client.json": meta} {
		tmp := filepath.Join(u.dir(), name+".tmp")
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, filepath.Join(u.dir(), name)); err != nil {
			return err
		}
	}
	u.mu.Lock()
	u.cl = cl
	u.mu.Unlock()
	return nil
}

// check downloads the PS5 client of the newest release when it is newer
// than the stored one.
func (u *ps5Updates) check(ctx context.Context) error {
	rel, err := LatestRelease(ctx)
	if err != nil {
		return err
	}
	if !rel.Has(PS5Asset) {
		return nil
	}
	if cl := u.current(); cl != nil && !NewerVersion(rel.Version, cl.Version) {
		return nil
	}
	elf, err := rel.Download(ctx, PS5Asset)
	if err != nil {
		return err
	}
	if err := u.store(rel.Version, elf); err != nil {
		return err
	}
	u.s.log.Info("ps5: new client build ready for consoles", "version", rel.Version, "bytes", len(elf))
	return nil
}

func (u *ps5Updates) run(ctx context.Context) {
	for {
		if err := u.check(ctx); err != nil && !errors.Is(err, context.Canceled) {
			u.s.log.Warn("update check", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(updateEvery):
		}
	}
}

// StorePS5Client makes elf the build offered to consoles (desktop app, tests).
func (s *Server) StorePS5Client(version string, elf []byte) error { return s.ps5up.store(version, elf) }

func (s *Server) handlePS5Client(w http.ResponseWriter, r *http.Request) {
	cl := s.ps5up.current()
	if cl == nil {
		writeErr(w, http.StatusNotFound, "no_build", "this server has no PS5 client build yet")
		return
	}
	writeJSON(w, http.StatusOK, cl)
}

func (s *Server) handlePS5ClientFile(w http.ResponseWriter, r *http.Request) {
	if s.ps5up.current() == nil {
		writeErr(w, http.StatusNotFound, "no_build", "this server has no PS5 client build yet")
		return
	}
	f, err := os.Open(filepath.Join(s.ps5up.dir(), "eboot.elf"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "no_build", err.Error())
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "eboot.elf", st.ModTime(), f)
}
