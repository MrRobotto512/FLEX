package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSameEpisodeTitle(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"Things Change", "Things Change", true},
		{"Puss Gets The Boot (1940)", "Puss Gets the Boot", true},
		{"Really You (Parts 1-2)", "Really You (1)", true},
		{"Queen Bea - Super Secret Secret Squirrel", "Queen Bea", false}, // too short to count as contained
		{"Rise of the Turtles", "Things Change", false},
		{"Pilot", "Pilot", false}, // every series has one
	} {
		if got := sameEpisodeTitle(c.a, c.b); got != c.want {
			t.Errorf("sameEpisodeTitle(%q, %q) = %v; want %v", c.a, c.b, got, c.want)
		}
	}
	for _, s := range []string{"Episode 5", "Pilot", "Intro", "Love", ""} {
		if usefulEpisodeTitle(s) {
			t.Errorf("usefulEpisodeTitle(%q) = true; too generic to pick a series by", s)
		}
	}
}

// Three series called "Teenage Mutant Ninja Turtles": the 2012 one is TMDB's
// top hit, the files are the 2003 one.
func TestPickShowByEpisodeTitle(t *testing.T) {
	seasons := map[string][]string{
		"/tv/51817/season/1": {"Rise of the Turtles", "Turtle Temper"},
		"/tv/1981/season/1":  {"Things Change", "A Better Mousetrap"},
		"/tv/1984/season/1":  {"Turtle Tracks", "Enter the Shredder"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		names, ok := seasons[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		var res struct {
			Episodes []map[string]string `json:"episodes"`
		}
		for _, n := range names {
			res.Episodes = append(res.Episodes, map[string]string{"name": n})
		}
		json.NewEncoder(w).Encode(res)
	}))
	defer srv.Close()
	defer func(old string) { tmdbAPI = old }(tmdbAPI)
	tmdbAPI = srv.URL

	tm := NewTMDB("test", t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	hits := []tmdbShow{
		{ID: 51817, Name: "Teenage Mutant Ninja Turtles", FirstAirDate: "2012-09-29"},
		{ID: 1984, Name: "Teenage Mutant Ninja Turtles", FirstAirDate: "1987-12-28"},
		{ID: 1981, Name: "Teenage Mutant Ninja Turtles", FirstAirDate: "2003-02-08"},
	}
	ctx := context.Background()
	if got := tm.pickShow(ctx, hits, EpisodeHint{Season: 1, Title: "Things Change"}); got.ID != 1981 {
		t.Errorf("picked %d; want the 2003 series (1981)", got.ID)
	}
	if got := tm.pickShow(ctx, hits, EpisodeHint{Season: 1, Title: "Rise of the Turtles"}); got.ID != 51817 {
		t.Errorf("picked %d; want the top hit when it has the episode", got.ID)
	}
	if got := tm.pickShow(ctx, hits, EpisodeHint{Season: 1, Title: "Something Else Entirely"}); got.ID != 51817 {
		t.Errorf("picked %d; want the top hit when no series has the episode", got.ID)
	}
	if got := tm.pickShow(ctx, hits, EpisodeHint{Season: 9, Title: "Things Change"}); got.ID != 51817 {
		t.Errorf("picked %d; want the top hit when no series has that season", got.ID)
	}
}
