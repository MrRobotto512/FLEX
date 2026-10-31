package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPeerURLs(t *testing.T) {
	pe := &peer{slot: 3}
	cases := map[string]string{
		"/api/v1/movies/42/poster":      "/api/v1/movies/300000042/poster",
		"/api/v1/episodes/7/still":      "/api/v1/episodes/300000007/still",
		"/api/v1/movies/42/file":        "/api/v1/movies/300000042/file",
		"/api/v1/shows/5?x=1":           "/api/v1/shows/300000005?x=1",
		"/api/v1/hls/abc123/index.m3u8": "/api/v1/hls/p3-abc123/index.m3u8",
		"":                              "",
		"https://image.tmdb.org/x.jpg":  "https://image.tmdb.org/x.jpg",
	}
	for in, want := range cases {
		if got := pe.url(in); got != want {
			t.Errorf("url(%q) = %q, want %q", in, got, want)
		}
	}
	if slot, id := splitID(pe.global(42)); slot != 3 || id != 42 {
		t.Errorf("splitID(global(42)) = %d, %d", slot, id)
	}
	if pe.global(peerStride) != 0 || pe.global(0) != 0 {
		t.Error("ids that don't fit must come out 0")
	}
	if (&peer{slot: maxPeerSlots}).global(peerStride-1) >= 1<<31 {
		t.Error("the last slot must stay below 2^31 for the PS5")
	}
}

// fakePeer is a linked server with one movie (42) and one show (7, episode 70).
type fakePeer struct {
	mu      sync.Mutex
	headers []http.Header
	got     []string // "METHOD path?query body"
}

func (f *fakePeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.headers = append(f.headers, r.Header.Clone())
	f.got = append(f.got, strings.TrimSpace(r.Method+" "+r.URL.RequestURI()+" "+string(body)))
	f.mu.Unlock()
	w.Header().Set("Access-Control-Allow-Origin", "*")
	js := func(v string) { w.Header().Set("Content-Type", "application/json"); io.WriteString(w, v) }
	switch r.Method + " " + r.URL.Path {
	case "GET /api/v1/server":
		js(`{"name":"Garage","id":"garage1","version":"0.4.0"}`)
	case "GET /api/v1/library/status":
		js(`{"movies":1,"shows":1}`)
	case "GET /api/v1/movies":
		js(`[{"id":42,"title":"Alien","poster":"/api/v1/movies/42/poster","added_at":"2026-10-01T00:00:00Z"}]`)
	case "GET /api/v1/shows":
		js(`[{"id":7,"title":"Futurama","poster":"/api/v1/shows/7/poster","episodes":1}]`)
	case "GET /api/v1/shows/7":
		js(`{"id":7,"title":"Futurama","season_list":[{"season":1,"episodes":[{"id":70,"show_id":7,"season":1,"episode":1,
			"title":"Space Pilot 3000","still":"/api/v1/episodes/70/still"}]}],"next_episode":{"id":70,"show_id":7}}`)
	case "GET /api/v1/continue":
		js(`[{"kind":"episode","id":70,"title":"Futurama","poster":"/api/v1/shows/7/poster","poster_of":"show",
			"poster_id":7,"show_id":7,"played":"` + time.Now().UTC().Format(time.RFC3339) + `"}]`)
	case "GET /api/v1/movies/42/play":
		js(`{"method":"hls","url":"/api/v1/hls/abc123/index.m3u8","duration_s":100}`)
	case "GET /api/v1/hls/abc123/index.m3u8":
		io.WriteString(w, "#EXTM3U\nseg0.ts\n")
	case "DELETE /api/v1/hls/abc123", "PUT /api/v1/movies/42/progress":
		w.WriteHeader(http.StatusNoContent)
	case "GET /api/v1/movies/42/file":
		http.ServeContent(w, r, "a.mp4", time.Time{}, bytes.NewReader([]byte("0123456789")))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakePeer) saw(want string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, g := range f.got {
		if strings.HasPrefix(g, want) {
			return true
		}
	}
	return false
}

func TestLinkedServer(t *testing.T) {
	d, err := OpenDB(filepath.Join(t.TempDir(), "flex.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := d.Exec(`INSERT INTO movies(id, path, title, size, mtime, added_at, seen_at) VALUES (1, 'heat.mkv', 'Heat', 1, 1, ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}

	fake := &fakePeer{}
	ps := httptest.NewServer(fake)
	defer ps.Close()

	s := &Server{cfg: Config{}, db: d, id: "front", log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.ps5 = newPS5Keeper(s)
	s.peers = newPeers(s)
	s.peers.check(context.Background(), []string{strings.TrimPrefix(ps.URL, "http://")})
	if info := s.Peers(); len(info) != 1 || info[0].Name != "Garage" || !info[0].Online || info[0].Movies != 1 {
		t.Fatalf("peer not linked: %+v", info)
	}
	h := s.Handler()

	call := func(method, path, from string, hdr map[string]string, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if from != "" {
			req.RemoteAddr = from
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	ps5 := map[string]string{"X-Flex-Client": "ps5"}

	var movies []Movie
	json.Unmarshal(call("GET", "/api/v1/movies", "", ps5, "").Body.Bytes(), &movies)
	if len(movies) != 2 || movies[0].Title != "Alien" || movies[0].ID != peerStride+42 ||
		movies[0].Poster != "/api/v1/movies/100000042/poster" || movies[1].ID != 1 {
		t.Errorf("merged movies: %+v", movies)
	}
	movies = nil
	json.Unmarshal(call("GET", "/api/v1/movies", "", map[string]string{peerHeader: "other"}, "").Body.Bytes(), &movies)
	if len(movies) != 1 || movies[0].ID != 1 {
		t.Errorf("a linked server must get local items only: %+v", movies)
	}

	var sh Show
	json.Unmarshal(call("GET", "/api/v1/shows/100000007", "", ps5, "").Body.Bytes(), &sh)
	if sh.ID != peerStride+7 || len(sh.SeasonList) != 1 || sh.SeasonList[0].Episodes[0].ID != peerStride+70 ||
		sh.SeasonList[0].Episodes[0].ShowID != peerStride+7 ||
		sh.SeasonList[0].Episodes[0].Still != "/api/v1/episodes/100000070/still" || sh.NextEpisode.ID != peerStride+70 {
		t.Errorf("linked show: %+v", sh)
	}

	var cont []ContinueItem
	json.Unmarshal(call("GET", "/api/v1/continue", "", ps5, "").Body.Bytes(), &cont)
	if len(cont) != 1 || cont[0].ID != peerStride+70 || cont[0].PosterID != peerStride+7 || cont[0].ShowID != peerStride+7 {
		t.Errorf("linked continue: %+v", cont)
	}

	// the profile is chosen where the client is: over the tailnet a PS5 gets "remote"
	var info PlayInfo
	json.Unmarshal(call("GET", "/api/v1/movies/100000042/play?caps=hevc", "100.70.1.2:4000", ps5, "").Body.Bytes(), &info)
	if info.URL != "/api/v1/hls/p1-abc123/index.m3u8" {
		t.Errorf("play url %q", info.URL)
	}
	if !fake.saw("GET /api/v1/movies/42/play?caps=hevc&profile=remote") {
		t.Errorf("play should go out with profile=remote: %q", fake.got)
	}
	call("GET", "/api/v1/movies/100000042/play", "192.168.0.50:4000", ps5, "")
	if !fake.saw("GET /api/v1/movies/42/play?profile=ps5") {
		t.Errorf("play from the LAN should go out with profile=ps5: %q", fake.got)
	}

	if rec := call("GET", "/api/v1/hls/p1-abc123/index.m3u8", "", ps5, ""); rec.Body.String() != "#EXTM3U\nseg0.ts\n" {
		t.Errorf("playlist: %d %q", rec.Code, rec.Body.String())
	} else if got := rec.Header().Values("Access-Control-Allow-Origin"); len(got) != 1 {
		t.Errorf("CORS header doubled: %q", got)
	}
	call("DELETE", "/api/v1/hls/p1-abc123", "", ps5, "")
	if !fake.saw("DELETE /api/v1/hls/abc123") {
		t.Errorf("stop not passed on: %q", fake.got)
	}
	rec := call("GET", "/api/v1/movies/100000042/file", "", map[string]string{"Range": "bytes=2-5"}, "")
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "2345" {
		t.Errorf("range: %d %q", rec.Code, rec.Body.String())
	}
	call("PUT", "/api/v1/movies/100000042/progress", "", ps5, `{"position_s":60,"duration_s":100}`)
	if !fake.saw(`PUT /api/v1/movies/42/progress {"position_s":60,"duration_s":100}`) {
		t.Errorf("progress not passed on: %q", fake.got)
	}

	if rec := call("GET", "/api/v1/movies/200000001", "", nil, ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown slot: %d", rec.Code)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, hd := range fake.headers {
		if hd.Get("X-Flex-Client") != "peer" || hd.Get(peerHeader) != "front" {
			t.Errorf("linked request headers: %v", hd)
		}
	}
}
