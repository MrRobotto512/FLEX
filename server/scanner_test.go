package server

import (
	"path/filepath"
	"testing"
)

func TestParseName(t *testing.T) {
	cases := []struct {
		in    string
		title string
		year  int
	}{
		{"Heat (1995)", "Heat", 1995},
		{"heat.1995.1080p.BluRay.x264", "Heat", 1995},
		{"Direct Play Test (2024)", "Direct Play Test", 2024},
		{"transcode.test.2023.2160p.x265", "Transcode Test", 2023},
		{"2001 A Space Odyssey 1968", "2001 A Space Odyssey", 1968},
		{"Some.Movie.1080p.WEB-DL", "Some Movie", 0},
		{"10 Things I Hate About You", "10 Things I Hate About You", 0},
	}
	for _, c := range cases {
		title, year := parseName(c.in)
		if title != c.title || year != c.year {
			t.Errorf("parseName(%q) = %q, %d; want %q, %d", c.in, title, year, c.title, c.year)
		}
	}
}

func TestParseEpisode(t *testing.T) {
	cases := []struct {
		in      string
		ok      bool
		series  string
		year    int
		season  int
		episode int
		title   string
	}{
		{"Bob's Burgers S09E01 Just One of the Boyz 4 Now for Now", true, "Bob's Burgers", 0, 9, 1, "Just One of the Boyz 4 Now for Now"},
		{"bobs.burgers.s09e02.the.taking.of.funtime.1080p.web-dl", true, "Bobs Burgers", 0, 9, 2, "The Taking Of Funtime"},
		{"The Expanse (2015) - S01E03 - Remember the Cant", true, "The Expanse", 2015, 1, 3, "Remember the Cant"},
		{"S01E01", true, "", 0, 1, 1, ""},
		{"show.1x02.pilot", true, "Show", 0, 1, 2, "Pilot"},
		{"Friends S10E17E18 The Last One", true, "Friends", 0, 10, 17, "The Last One"},
		{"Heat (1995)", false, "", 0, 0, 0, ""},
		{"Movie.1920x1080.BluRay", false, "", 0, 0, 0, ""},
		{"2 Fast 2 Furious (2003)", false, "", 0, 0, 0, ""},
	}
	for _, c := range cases {
		e, ok := parseEpisode(c.in)
		if ok != c.ok {
			t.Errorf("parseEpisode(%q) ok = %v; want %v", c.in, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if e.Series != c.series || e.Year != c.year || e.Season != c.season || e.Episode != c.episode || e.Title != c.title {
			t.Errorf("parseEpisode(%q) = %+v; want series %q year %d S%02dE%02d title %q",
				c.in, e, c.series, c.year, c.season, c.episode, c.title)
		}
	}
}

func TestSeriesFromDirs(t *testing.T) {
	// written with "/" and converted, so the paths split on this OS's
	// separator (a backslash is just a character in a Linux file name)
	root := filepath.FromSlash("/TV")
	cases := []struct {
		path   string
		series string
		year   int
	}{
		{"/TV/Bob's Burgers/Season 09/S09E01.mkv", "Bob's Burgers", 0},
		{"/TV/The Expanse (2015)/S01E01.mkv", "The Expanse", 2015},
		{"/TV/S01E01.mkv", "", 0},
	}
	for _, c := range cases {
		series, year := seriesFromDirs(filepath.FromSlash(c.path), root)
		if series != c.series || year != c.year {
			t.Errorf("seriesFromDirs(%q) = %q, %d; want %q, %d", c.path, series, year, c.series, c.year)
		}
	}
}
