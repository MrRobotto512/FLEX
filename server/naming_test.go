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

// Folder layouts seen in the same library after the first TV scan.
func TestTVShowFolders(t *testing.T) {
	root := filepath.FromSlash("D:/TV Shows")
	p := func(s string) string { return filepath.Join(root, filepath.FromSlash(s)) }
	cases := []struct {
		root, path      string
		series          string
		season, episode int
	}{
		{root, p("Bobs Burgers Season 3 Complete/Bobs.Burgers.S03E01.mkv"), "Bobs Burgers", 3, 1},
		{root, p("That 70's Show - Season 4/01 - That 70's Show - Hyde Moves In.avi"), "That 70's Show", 4, 1},
		{root, p("Better Call Saul Season 2 (1080p x265 Joy)/Better Call Saul S02E03.mkv"), "Better Call Saul", 2, 3},
		{root, p("Mamas Family Complete Series 1-6 SD TVRIP kellycarl/Season 2/03 Mama Learns to Drive.avi"), "Mamas Family", 2, 3},
		{root, p("Chappelle's Show The Complete Series/Season 1/101 - Episode One.avi"), "Chappelle's Show", 1, 1},
		{root, p("Futurama Complete/Season 1/Futurama 1x02 The Series Has Landed.avi"), "Futurama", 1, 2},
		{root, p("A Series of Unfortunate Events/S01E01.mkv"), "A Series of Unfortunate Events", 1, 1},
		// the show's own folder added as the TV folder
		{filepath.FromSlash("D:/TV/Workaholics"), filepath.FromSlash("D:/TV/Workaholics/Season 3 [DVD Rips]/E05 Fat Cats.avi"), "Workaholics", 3, 5},
		{root, p("2 stupid dogs/1994 Season 2 [Moonsong]/2 Stupid Dogs - 01a Stunt Dogs [Moonsong].avi"), "2 Stupid Dogs", 2, 1},
		{root, p("Bobs.Burgers.S07E19.720p.HDTV.x264-AVS[rarbg]/Bobs.Burgers.S07E19.720p.HDTV.x264-AVS.mkv"), "Bobs Burgers", 7, 19},
		{root, p("The Fresh Prince of Bel-Air/108+9 - Someday Your Prince Will Be In Effect.avi"), "The Fresh Prince of Bel-Air", 1, 8},
		{root, p("Mr.Bean Complete DVDs Collection/02 Good Night, MrBean DVDRip XviD.avi"), "Mr Bean", 1, 2},
		// double episodes
		{root, p("Will & Grace/Season 7/7x24 & 7x25 - It's A Dad, Dad, Dad, Dad World.avi"), "Will & Grace", 7, 24},
		{root, p("Will & Grace/Season 8/8x01 & 8x02 - Alive and Schticking.avi"), "Will & Grace", 8, 1},
	}
	for _, c := range cases {
		e := parseTVEpisode(c.path, c.root)
		if e.Series != c.series || e.Season != c.season || e.Episode != c.episode {
			t.Errorf("parseTVEpisode(%q)\n got  %q S%02dE%02d (%q)\n want %q S%02dE%02d",
				c.path, e.Series, e.Season, e.Episode, e.Title, c.series, c.season, c.episode)
		}
	}
	if _, ok := seasonFromDir("Mamas Family Complete Series 1-6"); ok {
		t.Error(`"Complete Series 1-6" is a range, not a season`)
	}
	if e, _ := parseEpisode("Friends S10E17E18 The Last One"); e.Title != "The Last One" {
		t.Errorf("double episode title = %q", e.Title)
	}
	if e, _ := parseEpisode("7x24 & 7x25 - It's A Dad, Dad, Dad, Dad World"); e.Series != "" || e.Title != "It's A Dad, Dad, Dad, Dad World" {
		t.Errorf("7x24 & 7x25: series %q title %q", e.Series, e.Title)
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
		{"www.HDSector.com - Zoolander.2001.BluRay.1080p.x264.AAC.5.1.-.Hon3y", "Zoolander", 2001},
		{"Crazy4TV.com - Spider-Man.Homecoming.2017.1080p.BluRay.x265-HETeam", "Spider-Man Homecoming", 2017},
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
