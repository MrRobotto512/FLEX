package server

import (
	"path/filepath"
	"testing"
)

// Real file names from a user's library (2026-10-30).

func TestParseTVEpisode(t *testing.T) {
	root := filepath.FromSlash("D:/TV Shows")
	p := func(s string) string { return filepath.Join(root, filepath.FromSlash(s)) }
	cases := []struct {
		path            string
		series          string
		season, episode int
		title           string
	}{
		{p("Futurama/Season 3/1 - Amazon women in the mood - Futurama - Season 3.avi"), "Futurama", 3, 1, "Amazon women in the mood"},
		{p("Futurama/Season 3/10 - Where the Buggalo Roam - Futurama - Season 3.avi"), "Futurama", 3, 10, "Where the Buggalo Roam"},
		{p("Futurama/FUT407 - Jurassic Bark.avi"), "Futurama", 4, 7, "Jurassic Bark"},
		{p("Will & Grace/119 - It Had To Be You.avi"), "Will & Grace", 1, 19, "It Had To Be You"},
		{p("Will & Grace/Season 6/614 - Breaking Up Is Hard To Do, Pt 1.avi"), "Will & Grace", 6, 14, "Breaking Up Is Hard To Do, Pt 1"},
		{p("The Office/Season 5/[5 02] Business Ethics.mkv"), "The Office", 5, 2, "Business Ethics"},
		{p("The Office/The Office [401] Fun Run.mkv"), "The Office", 4, 1, "Fun Run"},
		{p("The Office/The Office [3 09] The Convict.mkv"), "The Office", 3, 9, "The Convict"},
		{p("Workaholics/E05 Great Moments in Hookup History.mp4"), "Workaholics", 1, 5, "Great Moments in Hookup History"},
		{p("Goosebumps/Haunting Hour 1-04 The Dead Body.mp4"), "Goosebumps", 1, 4, "The Dead Body"},
		{p("It's Always Sunny in Philadelphia/Season 1/Its Always Sunny in Philadelphia Episode 02 - The Gang Gets Invincible.avi"), "It's Always Sunny in Philadelphia", 1, 2, "The Gang Gets Invincible"},
		{p("The Golden Girls/The Golden Girls 01 Blanche Delivers.mkv"), "The Golden Girls", 1, 1, "Blanche Delivers"},
		{p("2 Broke Girls/2 Broke Girls 507.mkv"), "2 Broke Girls", 5, 7, ""},
		{p("Mr Bean/01 MrBean.avi"), "Mr Bean", 1, 1, "MrBean"},
		{p("TMNT (2003)/Season 2/01 Turtles in Space (Part 1) The Fugitoid.avi"), "TMNT", 2, 1, "Turtles in Space (Part 1) The Fugitoid"},
		{p("Bob's Burgers/Season 09/Bobs.Burgers.S09E01.Just.One.of.the.Boyz.4.Now.for.Now.mkv"), "Bob's Burgers", 9, 1, "Just One of the Boyz 4 Now for Now"},
		{p("Tom And Jerry/Tom And Jerry - 002 - The Midnight Snack.avi"), "Tom And Jerry", 1, 2, "The Midnight Snack"},
		{p("Mystery Show/Some Unnumbered Special.mkv"), "Mystery Show", 1, 0, "Some Unnumbered Special"},
	}
	for _, c := range cases {
		e := parseTVEpisode(c.path, root)
		if e.Series != c.series || e.Season != c.season || e.Episode != c.episode || e.Title != c.title {
			t.Errorf("parseTVEpisode(%q)\n got  %q S%02dE%02d %q\n want %q S%02dE%02d %q",
				c.path, e.Series, e.Season, e.Episode, e.Title, c.series, c.season, c.episode, c.title)
		}
	}
}

func TestParseNameYearFirstAndIndex(t *testing.T) {
	cases := []struct {
		in    string
		title string
		year  int
	}{
		{"1967 - The Jungle Book", "The Jungle Book", 1967},
		{"1942 - How to Play Baseball", "How to Play Baseball", 1942},
		{"01 Indiana Jones And The Raiders Of The Lost Ark (1981)", "Indiana Jones And The Raiders Of The Lost Ark", 1981},
		{"01:08 (2019)", "01:08", 2019},
		{"2 Fast 2 Furious (2003)", "2 Fast 2 Furious", 2003},
		{"1917 (2019)", "1917", 2019},
	}
	for _, c := range cases {
		title, year := parseName(c.in)
		if title != c.title || year != c.year {
			t.Errorf("parseName(%q) = %q, %d; want %q, %d", c.in, title, year, c.title, c.year)
		}
	}
}

func TestTitleCandidates(t *testing.T) {
	contains := func(list []string, want string) bool {
		for _, s := range list {
			if s == want {
				return true
			}
		}
		return false
	}
	cases := []struct{ in, want string }{
		{"01 Rush Hour - Jackie Chan Action", "Rush Hour"},
		{"01 Star Wars Episode I The Phantom Menace - George Lucas", "Star Wars Episode I The Phantom Menace"},
		{"01 Friday The 13th Part 1 A Long Night At Camp Blood - Horror", "Friday The 13th"},
		{"Argo Extended Cut", "Argo"},
		{"The Fifth Element Remastered", "The Fifth Element"},
		{"Austin Powers 1", "Austin Powers"},
		{"Watch The Marvels", "The Marvels"},
		{"WwW SeeHD PL Goosebumps 2 Haunted Halloween", "Goosebumps 2 Haunted Halloween"},
		{"Freddy Vs Jason - Horror", "Freddy Vs Jason"},
	}
	for _, c := range cases {
		got := titleCandidates(c.in)
		if !contains(got, c.want) {
			t.Errorf("titleCandidates(%q) = %q; want it to contain %q", c.in, got, c.want)
		}
		if got[0] != c.in {
			t.Errorf("titleCandidates(%q) should try the title as-is first, got %q", c.in, got[0])
		}
	}
}

func TestLooksLikeExtra(t *testing.T) {
	for name, want := range map[string]bool{
		"Movie-sample.mkv":             true,
		"00 Deleted Scenes.mkv":        true,
		"00 Animatics.avi":             true,
		"Trailer Park Boys S01E01.mkv": false,
		"The Sample Movie (2001).mkv":  false,
		"Behind the Scenes.mp4":        true,
	} {
		if got := looksLikeExtra(name); got != want {
			t.Errorf("looksLikeExtra(%q) = %v; want %v", name, got, want)
		}
	}
}
