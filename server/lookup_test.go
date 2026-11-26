package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The PS5 asks for a USB title's poster by name; the server finds it on
// TMDB once and remembers.
func TestLookupPoster(t *testing.T) {
	searches := 0
	tmdb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/search/movie":
			searches++
			res := map[string]any{"results": []any{}}
			if strings.EqualFold(r.URL.Query().Get("query"), "Ambulance") {
				res["results"] = []any{map[string]any{"id": 763285, "title": "Ambulance", "release_date": "2022-03-16"}}
			}
			json.NewEncoder(w).Encode(res)
		case r.URL.Path == "/movie/763285":
			json.NewEncoder(w).Encode(map[string]any{"id": 763285, "title": "Ambulance", "release_date": "2022-03-16", "poster_path": "/amb.jpg"})
		case strings.HasSuffix(r.URL.Path, "/amb.jpg"):
			w.Write([]byte("JPEG poster"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer tmdb.Close()
	defer func(a, b string) { tmdbAPI, tmdbImage = a, b }(tmdbAPI, tmdbImage)
	tmdbAPI, tmdbImage = tmdb.URL, tmdb.URL

	s := &Server{log: slog.Default()}
	s.tmdb = NewTMDB("key", t.TempDir(), s.log)
	get := func(query string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.handleLookupPoster(rec, httptest.NewRequest(http.MethodGet, "/api/v1/lookup/poster?"+query, nil))
		return rec
	}
	for _, name := range []string{"Ambulance+2022", "Ambulance.2022.1080p.WEBRip.x264-RARBG.mp4", "Ambulance+2022"} {
		if rec := get("kind=movie&name=" + name); rec.Code != http.StatusOK || rec.Body.String() != "JPEG poster" {
			t.Errorf("%s: HTTP %d %q", name, rec.Code, rec.Body.String())
		}
	}
	before := searches
	if rec := get("kind=movie&name=Ambulance+2022"); rec.Code != http.StatusOK || searches != before {
		t.Errorf("a name seen before should not ask TMDB again (%d -> %d searches)", before, searches)
	}
	if rec := get("kind=movie&name=No+Such+Film+1999"); rec.Code != http.StatusNotFound {
		t.Errorf("no match: HTTP %d", rec.Code)
	}
	if rec := get("kind=game&name=x"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad kind: HTTP %d", rec.Code)
	}
	s.tmdb = nil
	if rec := get("kind=movie&name=Ambulance+2022"); rec.Code != http.StatusNotFound {
		t.Errorf("no TMDB key: HTTP %d", rec.Code)
	}
}
