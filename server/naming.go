package server

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// File and folder name parsing: movie titles and years, TV episodes.

var (
	yearRe      = regexp.MustCompile(`^(.+?)[\s._\-(\[]+((?:19|20)\d{2})(?:[\s._\-)\]]|$)`)
	yearFirstRe = regexp.MustCompile(`^((?:19|20)\d{2})\s*[-.]\s+(.+)$`) // "1967 - The Jungle Book"
	indexRe     = regexp.MustCompile(`^0\d\s*[-.)]?\s+`)                 // "01 Indiana Jones…" (zero-padded only)
	noiseRe     = regexp.MustCompile(`(?i)[\s._\-](1080p|2160p|720p|480p|4k|uhd|bluray|blu-ray|bdrip|brrip|web-?dl|webrip|hdtv|dvdrip|x264|x265|h\.?264|h\.?265|hevc|remux|hdr|dv|atmos|dts|aac|ac3|proper|repack)\b.*$`)
	spaceRe     = regexp.MustCompile(`[._]+|\s+`)

	// "Show Name S01E02 Episode Title", "show.name.1x02", "S01E01E02", with
	// an optional "(2011)" after the show name
	episodeRe = regexp.MustCompile(`(?i)^(?:(.*?)[\s._\-]+)?(?:S(\d{1,2})[\s._\-]*E(\d{1,3})(?:[\s._\-]*E\d{1,3})*|(\d{1,2})x(\d{2,3}))(?:[\s._\-]+(.*))?$`)

	// looser episode numbering, only trusted inside TV folders
	bracketRe = regexp.MustCompile(`\[(\d{1,2})[\s.x]?(\d{2})\]`)              // [5 02] [401] [3x09]
	codeRe    = regexp.MustCompile(`^[A-Za-z]{2,5}(\d{1,2})(\d{2})\b`)         // FUT407
	epWordRe  = regexp.MustCompile(`(?i)\b(?:episode|ep)[\s._\-]*(\d{1,3})\b`) // Episode 02, Ep.5
	eNumRe    = regexp.MustCompile(`(?i)^e(\d{1,3})\b`)                        // E05 Title
	dashSERe  = regexp.MustCompile(`\b(\d{1,2})-(\d{2})\b`)                    // 1-04 Title
	leadNumRe = regexp.MustCompile(`^(\d{1,4})(?:\s*[-._)]\s*|\s+|$)`)         // 01 Title, 1 - Title, 119 - Title, 507

	seasonWordRe = regexp.MustCompile(`(?i)\b(?:season|series|staffel)[\s._\-]*(\d{1,2})\b`)
	seasonCodeRe = regexp.MustCompile(`(?i)^s(\d{1,2})$`)
	specialsRe   = regexp.MustCompile(`(?i)^specials?$`)
	seasonSufRe  = regexp.MustCompile(`(?i)\s*-?\s*\b(?:season|series)\s*\d+\s*$`)

	// folders whose contents are bonus material, not episodes or movies
	extrasDirRe = regexp.MustCompile(`(?i)^(extras?|featurettes?|behind the scenes|deleted scenes|bonus( features)?|trailers?|samples?|interviews?|shorts)$`)
)

// ---------------------------------------------------------------- movies

// guessTitle prefers "Title (Year)" from the file name, then from the parent
// folder (common layout: "Heat (1995)/heat.mkv"), then falls back to the bare name.
func guessTitle(path, root string) (string, int) {
	t, y := parseName(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	if y != 0 {
		return t, y
	}
	parent := filepath.Dir(path)
	if filepath.Clean(parent) != filepath.Clean(root) {
		if pt, py := parseName(filepath.Base(parent)); py != 0 {
			return pt, py
		}
	}
	return t, 0
}

func parseName(name string) (string, int) {
	title, year := name, 0
	if m := yearFirstRe.FindStringSubmatch(name); m != nil {
		year, _ = strconv.Atoi(m[1])
		title = noiseRe.ReplaceAllString(" "+m[2], "")
	} else if m := yearRe.FindStringSubmatch(name); m != nil {
		title = m[1]
		year, _ = strconv.Atoi(m[2])
	} else {
		title = noiseRe.ReplaceAllString(title, "")
	}
	title = indexRe.ReplaceAllString(spaceRe.ReplaceAllString(title, " "), "")
	title = strings.TrimSpace(strings.Trim(strings.TrimSpace(title), "-"))
	if title == "" {
		title = name
	}
	if title == strings.ToLower(title) {
		title = titleCase(title) // "transcode test" -> "Transcode Test"
	}
	return title, year
}

var (
	candIndexRe   = regexp.MustCompile(`^\d{1,3}\s*[-.)]?\s+`)
	candEditionRe = regexp.MustCompile(`(?i)\s*[(\[]?\b(extended|director'?s|theatrical|unrated|uncut|special|collector'?s|anniversary|ultimate|final|remastered|restored|imax)\b(\s+(cut|edition|version))?[)\]]?|\s*\b(reboot|remastered)\b`)
	candPrefixRe  = regexp.MustCompile(`(?i)^(watch\s+|www\S*\s+\S+\s+[a-z]{2}\s+|disney'?s?\s+)`)
	candPartRe    = regexp.MustCompile(`(?i)^(.+?)\s+part\s+(\d+|one|i)\b.*$`)
	candTrailNum  = regexp.MustCompile(`\s+\d$`)
)

// titleCandidates lists search strings for a messy file title, most literal
// first: "01 Rush Hour - Jackie Chan Action" -> "Rush Hour - Jackie Chan
// Action" -> "Rush Hour". The first one TMDB knows wins.
func titleCandidates(title string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "-:"))
		if k := strings.ToLower(s); s != "" && !seen[k] && len(out) < 6 {
			seen[k] = true
			out = append(out, s)
		}
	}
	add(title)
	t := candIndexRe.ReplaceAllString(title, "")
	add(t)
	t = candPrefixRe.ReplaceAllString(t, "")
	add(t)
	if i := strings.Index(t, " - "); i > 0 {
		t = t[:i]
		add(t)
	}
	t = strings.Join(strings.Fields(candEditionRe.ReplaceAllString(t, " ")), " ")
	add(t)
	if m := candPartRe.FindStringSubmatch(t); m != nil {
		add(m[1])
	}
	add(candTrailNum.ReplaceAllString(t, ""))
	return out
}

// ---------------------------------------------------------------- TV

type episodeName struct {
	Series  string
	Year    int
	Season  int
	Episode int
	Title   string
}

// parseEpisode recognises the unambiguous forms (S01E02, 1x02) anywhere.
func parseEpisode(name string) (episodeName, bool) {
	m := episodeRe.FindStringSubmatch(name)
	if m == nil {
		return episodeName{}, false
	}
	var e episodeName
	if m[2] != "" {
		e.Season, _ = strconv.Atoi(m[2])
		e.Episode, _ = strconv.Atoi(m[3])
	} else {
		e.Season, _ = strconv.Atoi(m[4])
		e.Episode, _ = strconv.Atoi(m[5])
	}
	if strings.TrimSpace(m[1]) != "" {
		e.Series, e.Year = parseName(m[1])
	}
	e.Title = cleanEpisodeTitle(m[6], "")
	return e, true
}

// seasonFromDir reads "Season 3", "Series 03", "S03", "Futurama Season 3",
// "Specials" (season 0).
func seasonFromDir(name string) (int, bool) {
	if specialsRe.MatchString(name) {
		return 0, true
	}
	for _, re := range []*regexp.Regexp{seasonCodeRe, seasonWordRe} {
		if m := re.FindStringSubmatch(name); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n, true
		}
	}
	return 0, false
}

// seriesFromDirs handles "Show Name/Season 01/S01E01.mkv": the show is the
// folder above the season folder (never the library root itself).
func seriesFromDirs(path, root string) (string, int) {
	dir := filepath.Dir(path)
	if _, ok := seasonFromDir(filepath.Base(dir)); ok {
		dir = filepath.Dir(dir)
	}
	if filepath.Clean(dir) == filepath.Clean(root) {
		return "", 0
	}
	return parseName(filepath.Base(dir))
}

// inSeasonFolder reports whether any folder between root and the file is a
// season folder; such files are episodes even in a movie library.
func inSeasonFolder(path, root string) bool {
	for _, d := range relDirs(path, root) {
		if _, ok := seasonFromDir(d); ok {
			return true
		}
	}
	return false
}

func relDirs(path, root string) []string {
	rel, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil || rel == "." {
		return nil
	}
	return strings.Split(rel, string(filepath.Separator))
}

// parseTVEpisode names a file inside a TV folder. The show is the folder
// above a season folder, else the first folder under the library root, else
// whatever the file name says; the season comes from the file name, a season
// folder, or defaults to 1; the episode number from any of the common
// numbering styles. It always returns something: an unnumbered file becomes
// episode 0 of its season.
func parseTVEpisode(path, root string) episodeName {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	dirs := relDirs(path, root)
	season, haveSeason, showDir := 1, false, ""
	for i, d := range dirs {
		if n, ok := seasonFromDir(d); ok {
			season, haveSeason = n, true
			if i > 0 {
				showDir = dirs[i-1]
			}
		}
	}
	if showDir == "" && len(dirs) > 0 {
		showDir = dirs[0]
	}

	var e episodeName
	if showDir != "" {
		e.Series, e.Year = parseName(showDir)
	}
	if strict, ok := parseEpisode(base); ok {
		if e.Series == "" {
			e.Series, e.Year = strict.Series, strict.Year
		}
		e.Season, e.Episode, e.Title = strict.Season, strict.Episode, strict.Title
		return e
	}

	name := strings.TrimSpace(spaceRe.ReplaceAllString(stripShowPrefix(base, e.Series), " "))
	e.Season = season
	rest := name
	switch {
	case bracketRe.MatchString(name):
		m := bracketRe.FindStringSubmatchIndex(name)
		e.Season, _ = strconv.Atoi(name[m[2]:m[3]])
		e.Episode, _ = strconv.Atoi(name[m[4]:m[5]])
		rest = name[m[1]:]
	case codeRe.MatchString(name):
		m := codeRe.FindStringSubmatchIndex(name)
		e.Season, _ = strconv.Atoi(name[m[2]:m[3]])
		e.Episode, _ = strconv.Atoi(name[m[4]:m[5]])
		rest = name[m[1]:]
	case epWordRe.MatchString(name):
		m := epWordRe.FindStringSubmatchIndex(name)
		e.Episode, _ = strconv.Atoi(name[m[2]:m[3]])
		rest = name[m[1]:]
	case eNumRe.MatchString(name):
		m := eNumRe.FindStringSubmatchIndex(name)
		e.Episode, _ = strconv.Atoi(name[m[2]:m[3]])
		rest = name[m[1]:]
	case dashSERe.MatchString(name):
		m := dashSERe.FindStringSubmatchIndex(name)
		e.Season, _ = strconv.Atoi(name[m[2]:m[3]])
		e.Episode, _ = strconv.Atoi(name[m[4]:m[5]])
		rest = name[m[1]:]
	case leadNumRe.MatchString(name):
		m := leadNumRe.FindStringSubmatchIndex(name)
		digits := name[m[2]:m[3]]
		n, _ := strconv.Atoi(digits)
		rest = name[m[1]:]
		switch {
		// 3-4 digits encode season and episode: 119 = S1E19, 1012 = S10E12
		case len(digits) >= 3 && n/100 >= 1 && n%100 >= 1 && (!haveSeason || n/100 == season):
			e.Season, e.Episode = n/100, n%100
		default:
			e.Episode = n
		}
	}
	if e.Series == "" {
		e.Series = "Unknown show"
	}
	e.Title = cleanEpisodeTitle(rest, e.Series)
	return e
}

// stripShowPrefix removes the series name from the start of a file name
// ("The Golden Girls 01 Blanche Delivers" -> "01 Blanche Delivers"),
// comparing letters and digits only so punctuation differences don't matter.
func stripShowPrefix(name, series string) string {
	want := []rune(normKey(series))
	if len(want) == 0 {
		return name
	}
	alts := [][]rune{want}
	if strings.HasPrefix(string(want), "the") && len(want) > 3 {
		alts = append(alts, want[3:])
	}
	for _, w := range alts {
		got := 0
		for i, r := range name {
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
				continue
			}
			if unicode.ToLower(r) != w[got] {
				break
			}
			got++
			if got == len(w) {
				cut := i + len(string(r))
				next := strings.TrimLeft(name[cut:], " ._-")
				if cut < len(name) && (unicode.IsLetter(rune(name[cut])) || unicode.IsDigit(rune(name[cut]))) {
					break // "Friends" must not eat "Friendship"
				}
				return next
			}
		}
	}
	return name
}

func normKey(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, s)
}

func cleanEpisodeTitle(s, series string) string {
	s = noiseRe.ReplaceAllString(" "+s, "")
	s = spaceRe.ReplaceAllString(s, " ")
	s = strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "-"))
	s = strings.TrimSpace(seasonSufRe.ReplaceAllString(s, ""))
	// "Amazon women in the mood - Futurama" -> "Amazon women in the mood"
	if series != "" {
		if i := strings.LastIndex(s, " - "); i > 0 && normKey(s[i+3:]) == normKey(series) {
			s = s[:i]
		}
	}
	s = strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "-"))
	if s != "" && s == strings.ToLower(s) {
		s = titleCase(s)
	}
	return s
}

func titleCase(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

var extraFileRe = regexp.MustCompile(`(?i)(^|[\s._\-])(sample|trailer|deleted scenes|animatics|featurette|behind the scenes)$`)

// looksLikeExtra skips bonus files ("Movie-sample.mkv", "00 Deleted Scenes")
// without catching titles like "Trailer Park Boys".
func looksLikeExtra(name string) bool {
	return extraFileRe.MatchString(strings.TrimSuffix(name, filepath.Ext(name)))
}
