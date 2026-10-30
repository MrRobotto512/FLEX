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

	// "Show Name S01E02 Episode Title", "show.name.1x02": the first such token
	// splits the name into series and episode title
	episodeTokRe = regexp.MustCompile(`(?i)\bS(\d{1,2})[\s._\-]*E(\d{1,3})|\b(\d{1,2})x(\d{2,3})\b`)
	// the rest of a multi-episode name: "S01E01E02", "S01E01-E02", "7x24 & 7x25"
	moreEpsRe = regexp.MustCompile(`(?i)^(?:[\s._]*(?:-\s*)?E\d{1,3}\b|\s*[&+,-]\s*(?:\d{1,2}x\d{2,3}|S\d{1,2}E\d{1,3}|E\d{1,3})\b)+`)

	// looser episode numbering, only trusted inside TV folders
	bracketRe  = regexp.MustCompile(`\[(\d{1,2})[\s.x]?(\d{2})\]`)                          // [5 02] [401] [3x09]
	codeRe     = regexp.MustCompile(`^[A-Za-z]{2,5}(\d{1,2})(\d{2})\b`)                     // FUT407
	epWordRe   = regexp.MustCompile(`(?i)\b(?:episode|ep)[\s._\-]*(\d{1,3})\b`)             // Episode 02, Ep.5
	eNumRe     = regexp.MustCompile(`(?i)^e(\d{1,3})\b`)                                    // E05 Title
	dashSERe   = regexp.MustCompile(`\b(\d{1,2})-(\d{2})\b`)                                // 1-04 Title
	leadNumRe  = regexp.MustCompile(`^(\d{1,4})(?:\+\d{1,3}|-?[a-d]\b)?(?:\s*[-._)]\s*|\s+|$)`) // 01 Title, 1 - Title, 119 - Title, 507, 108+9 - Title, 01a Title, 201-a Title
	groupTagRe = regexp.MustCompile(`\s*\[[^\]]*\]\s*$`)                                    // "Stunt Dogs [Moonsong]"

	seasonWordRe = regexp.MustCompile(`(?i)\b(?:season|series|staffel)[\s._\-]*(\d{1,2})\b(\s*-\s*\d)?`)
	seasonCodeRe = regexp.MustCompile(`(?i)^s(\d{1,2})$`)
	specialsRe   = regexp.MustCompile(`(?i)^(?:\d+\s*-\s*)?(?:specials?|shorts)$`) // "Specials", "0000 - Shorts"
	onlyDigitsRe = regexp.MustCompile(`^\d+$`)
	airYearsRe   = regexp.MustCompile(`^[\d\s._\-]*$`) // what's left of "1993-1994 - Season 1" before "Season"
	// movie-list folders that obviously hold TV
	tvRootNameRe = regexp.MustCompile(`(?i)^(tv|tv[\s._-]*shows?|tv[\s._-]*series|television|shows|series)$`)
	seasonSufRe  = regexp.MustCompile(`(?i)\s*-?\s*\b(?:season|series)\s*\d+\s*$`)

	// where a show folder name stops being the show's name: "Bobs Burgers
	// Season 3 Complete", "Chappelle's Show The Complete Series", "Mamas
	// Family Complete Series 1-6 SD TVRIP", "Better Call Saul Season 2 (1080p"
	showCutRe = regexp.MustCompile(`(?i)\b(?:the\s+)?complete(?:\s+(?:series|seasons?|collection|box\s*set))?\b|\b(?:season|series|staffel)s?[\s._\-]*\d|\bS\d{1,2}E\d{1,3}|\bS\d{1,2}(?:\s*-\s*S?\d{1,2})?\b|\b(?:sd|tvrip|dvd\s?rips?|dvdrip|hdtv|web-?dl|webrip|bluray|1080p|720p|480p|x264|x265)\b|\s[\[(]`)

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
	if m := candSiteRe.FindStringSubmatch(title); m != nil { // "www HDSector com - Zoolander"
		title = m[1]
	}
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
	// "www HDSector com - Zoolander", "Crazy4TV com - Spider-Man Homecoming"
	candSiteRe = regexp.MustCompile(`(?i)^(?:www\s+)?\S+\s+(?:com|net|org|to|tv|me|cc|io|pl|ru|in)\s*-\s*(.+)$`)
	// "A Killer on Floor 32 l Watch the FULL Documentary" ("|" saved as "l")
	candPipeRe   = regexp.MustCompile(`\s+[|l]\s+`)
	candPartRe   = regexp.MustCompile(`(?i)^(.+?)\s+part\s+(\d+|one|i)\b.*$`)
	candTrailNum = regexp.MustCompile(`\s+\d$`)
)

// titleCandidates lists search strings for a messy file title, most literal
// first: "01 Rush Hour - Jackie Chan Action" -> "Rush Hour - Jackie Chan
// Action" -> "Rush Hour". The first one TMDB knows wins.
func titleCandidates(title string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "-:"))
		if k := strings.ToLower(s); s != "" && !seen[k] && len(out) < 8 {
			seen[k] = true
			out = append(out, s)
		}
	}
	add(title)
	t := candIndexRe.ReplaceAllString(title, "")
	add(t)
	if m := candSiteRe.FindStringSubmatch(t); m != nil {
		t = m[1]
		add(t)
	}
	t = candPrefixRe.ReplaceAllString(t, "")
	add(t)
	if loc := candPipeRe.FindStringIndex(t); loc != nil {
		t = t[:loc[0]]
		add(t)
	}
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
	// a wrong word in a long title ("…The True Story…" for "…The Untold
	// Story…"): the first words plus the year usually still find it
	if w := strings.Fields(t); len(w) >= 4 {
		add(strings.Join(w[:2], " "))
	}
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
	m := episodeTokRe.FindStringSubmatchIndex(name)
	if m == nil {
		return episodeName{}, false
	}
	num := func(i int) int { n, _ := strconv.Atoi(name[m[i]:m[i+1]]); return n }
	var e episodeName
	if m[2] >= 0 {
		e.Season, e.Episode = num(2), num(4)
	} else {
		e.Season, e.Episode = num(6), num(8)
	}
	if prefix := strings.Trim(name[:m[0]], " ._-[("); prefix != "" {
		e.Series, e.Year = parseName(prefix)
	}
	e.Title = cleanEpisodeTitle(moreEpsRe.ReplaceAllString(name[m[1]:], ""), "")
	return e, true
}

// seasonFromDir reads "Season 3", "Series 03", "S03", "Futurama Season 3",
// "Specials" (season 0).
func seasonFromDir(name string) (int, bool) {
	if specialsRe.MatchString(name) {
		return 0, true
	}
	if m := seasonCodeRe.FindStringSubmatch(name); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n, true
	}
	// "Season 3", but not "Complete Series 1-6"
	if m := seasonWordRe.FindStringSubmatch(name); m != nil && m[2] == "" {
		n, _ := strconv.Atoi(m[1])
		return n, true
	}
	return 0, false
}

// cleanShowName turns a show folder name into the series title. It returns
// "" when the folder is only a season ("Season 3 [DVD Rips]").
func cleanShowName(dir string) (string, int) {
	if specialsRe.MatchString(dir) {
		return "", 0
	}
	// "1993-1994 - Season 1": a season folder named with its air years, which
	// parseName would otherwise read as a title "1993" from 1994
	if loc := seasonWordRe.FindStringIndex(dir); loc != nil && airYearsRe.MatchString(dir[:loc[0]]) {
		if _, ok := seasonFromDir(dir); ok {
			return "", 0
		}
	}
	name, year := parseName(dir)
	if loc := showCutRe.FindStringIndex(name); loc != nil {
		cut := strings.TrimSpace(strings.Trim(strings.TrimSpace(name[:loc[0]]), "-._,"))
		if onlyDigitsRe.MatchString(cut) { // "1994 Season 2 [Moonsong]" is a season, not a show called 1994
			cut = ""
		}
		if cut == "" {
			if _, ok := seasonFromDir(name); ok {
				return "", 0
			}
			return name, year // "Complete Savages" is a title, not noise
		}
		name = cut
	}
	return name, year
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
	season, haveSeason, seasonIdx := 1, false, -1
	for i, d := range dirs {
		if n, ok := seasonFromDir(d); ok {
			season, haveSeason, seasonIdx = n, true, i
		}
	}

	// The show is the nearest folder above the season folder that is more
	// than a season ("That 70's Show - Season 4" is both); without a season
	// folder, the first folder under the root. A season folder right under
	// the root means the root itself is the show's folder.
	var e episodeName
	last := len(dirs) - 1
	if seasonIdx >= 0 {
		last = seasonIdx
	}
	if seasonIdx < 0 && len(dirs) > 0 {
		last = 0
	}
	for i := last; i >= 0 && e.Series == ""; i-- {
		e.Series, e.Year = cleanShowName(dirs[i])
	}
	if e.Series == "" && seasonIdx >= 0 {
		e.Series, e.Year = cleanShowName(filepath.Base(root))
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
	s = groupTagRe.ReplaceAllString(s, "")
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
