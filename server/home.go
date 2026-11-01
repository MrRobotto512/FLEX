package server

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"sort"
	"time"
)

// HomeCard is one poster on the home screen: a movie or a show.
type HomeCard struct {
	Kind     string   `json:"kind"` // "movie" | "show"
	ID       int64    `json:"id"`
	Title    string   `json:"title"`
	Year     int      `json:"year,omitempty"`
	Poster   string   `json:"poster,omitempty"`
	Backdrop string   `json:"backdrop,omitempty"`
	Overview string   `json:"overview,omitempty"`
	Genres   []string `json:"genres,omitempty"`
	Watched  bool     `json:"watched,omitempty"`
	ResumeS  int      `json:"resume_s,omitempty"`
	RuntimeS int      `json:"runtime_s,omitempty"`
	Seasons  int      `json:"seasons,omitempty"`

	tmdb  string // "m603" / "s1399": how the AI's rows name it
	added time.Time
}

type HomeRow struct {
	Title string     `json:"title"`
	Items []HomeCard `json:"items"`
}

// Home answers GET /home: a featured title, Continue Watching and the rows.
type Home struct {
	Hero     *HomeCard      `json:"hero,omitempty"`
	Continue []ContinueItem `json:"continue"`
	Rows     []HomeRow      `json:"rows"`
	Themes   string         `json:"themes"` // who made the themed rows: "Claude", "ChatGPT", "Gemini" or "TMDB"
}

// library is every movie and show this server lists, the linked servers'
// included.
func (s *Server) library(ctx context.Context) ([]Movie, []Show) {
	movies, _ := s.db.ListMovies("", "")
	shows, _ := s.db.ListShows("")
	if s.peers != nil {
		movies = s.peerMovies(ctx, movies, "")
		shows = s.peerShows(ctx, shows, "")
	}
	return movies, shows
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	var movies []Movie
	var shows []Show
	if s.linking(r) {
		movies, shows = s.library(r.Context())
	} else {
		movies, _ = s.db.ListMovies("", "")
		shows, _ = s.db.ListShows("")
	}
	cont, err := s.db.ContinueWatching(20)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	if s.linking(r) {
		cont = s.linkedContinue(r, cont, 20)
	}
	var th *Themes
	if s.themes != nil {
		th = s.themes.current()
	}
	writeJSON(w, http.StatusOK, buildHome(movies, shows, cont, th, time.Now(), nil))
}

// handleThemes hands this server's AI rows to linked servers without a key.
func (s *Server) handleThemes(w http.ResponseWriter, r *http.Request) {
	var th *Themes
	if s.themes != nil {
		th = s.themes.current()
	}
	if th == nil {
		writeErr(w, http.StatusNotFound, "not_found", "no themed rows yet")
		return
	}
	writeJSON(w, http.StatusOK, th)
}

// buildHome lays out the home screen. The AI's rows come first (the first
// one is the time of year), then Recently Added, TV Shows and genres; without
// AI rows a row for the time of year comes from TMDB keywords and genres.
// rnd picks the featured title (nil: a new one each time).
func buildHome(movies []Movie, shows []Show, cont []ContinueItem, th *Themes, now time.Time, rnd *rand.Rand) Home {
	if rnd == nil {
		rnd = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	}
	// the same title on two servers is shown once, the lower id (this server's) first
	sort.SliceStable(movies, func(i, j int) bool { return movies[i].ID < movies[j].ID })
	sort.SliceStable(shows, func(i, j int) bool { return shows[i].ID < shows[j].ID })
	var cards []HomeCard
	byTMDB := map[string]HomeCard{}
	add := func(c HomeCard) {
		if c.tmdb != "" {
			if _, dup := byTMDB[c.tmdb]; dup {
				return
			}
			byTMDB[c.tmdb] = c
		}
		cards = append(cards, c)
	}
	holidays := map[string][]HomeCard{}
	for _, m := range movies {
		c := HomeCard{Kind: "movie", ID: m.ID, Title: m.Title, Year: m.Year, Poster: m.Poster, Backdrop: m.Backdrop,
			Overview: m.Overview, Genres: m.Genres, Watched: m.Watched, ResumeS: m.ResumeS, RuntimeS: m.RuntimeS, added: m.AddedAt}
		if m.TMDBID != 0 {
			c.tmdb = fmt.Sprintf("m%d", m.TMDBID)
		}
		n := len(cards)
		if add(c); len(cards) > n {
			for _, h := range m.Holidays {
				holidays[h] = append(holidays[h], c)
			}
		}
	}
	nMovies := len(cards)
	for _, sh := range shows {
		c := HomeCard{Kind: "show", ID: sh.ID, Title: sh.Title, Year: sh.Year, Poster: sh.Poster, Backdrop: sh.Backdrop,
			Overview: sh.Overview, Genres: sh.Genres, Seasons: sh.Seasons, added: sh.AddedAt}
		if sh.TMDBID != 0 {
			c.tmdb = fmt.Sprintf("s%d", sh.TMDBID)
		}
		n := len(cards)
		if add(c); len(cards) > n {
			for _, h := range sh.Holidays {
				holidays[h] = append(holidays[h], c)
			}
		}
	}
	movieCards, showCards := cards[:nMovies], cards[nMovies:]

	// the same shuffle all day, so the PS5 and a browser agree
	y, m, d := now.Date()
	daily := rand.New(rand.NewPCG(uint64(y*10000+int(m)*100+d), 7))
	shuffled := func(in []HomeCard) []HomeCard {
		out := slices.Clone(in)
		daily.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
		return out
	}

	home := Home{Continue: cont, Themes: "TMDB"}
	if home.Continue == nil {
		home.Continue = []ContinueItem{}
	}
	var themed []HomeRow
	if th != nil {
		for _, r := range th.Rows {
			var items []HomeCard
			for _, id := range r.Items {
				if c, ok := byTMDB[id]; ok {
					items = append(items, c)
				}
			}
			if len(items) >= 4 {
				themed = append(themed, HomeRow{r.Title, items})
			}
		}
		if len(themed) > 0 {
			home.Themes = th.By
		}
	}
	if len(themed) == 0 {
		if name, holiday, genre := season(now); name != "" {
			items := slices.Clone(holidays[holiday])
			for _, c := range shuffled(movieCards) {
				if genre != "" && slices.Contains(c.Genres, genre) && !slices.ContainsFunc(items, func(o HomeCard) bool { return o.Kind == c.Kind && o.ID == c.ID }) {
					items = append(items, c)
				}
			}
			if len(items) >= 4 {
				themed = append(themed, HomeRow{name, items[:min(30, len(items))]})
			}
		}
	}

	newest := func(in []HomeCard) []HomeCard {
		out := slices.Clone(in)
		sort.SliceStable(out, func(i, j int) bool { return out[i].added.After(out[j].added) })
		return out[:min(30, len(out))]
	}
	rows := []HomeRow{}
	keep := func(title string, items []HomeCard) {
		if len(items) >= 4 {
			rows = append(rows, HomeRow{title, items})
		}
	}
	if len(themed) > 0 {
		rows = append(rows, themed[0])
	}
	keep("Recently Added", newest(movieCards))
	if len(themed) > 1 {
		rows = append(rows, themed[1:]...)
	}
	keep("TV Shows", newest(showCards))

	// genre rows, the biggest genres first: a few under AI rows, more without
	count := map[string]int{}
	for _, c := range movieCards {
		for _, g := range c.Genres {
			count[g]++
		}
	}
	var genres []string
	for g, n := range count {
		if n >= 8 {
			genres = append(genres, g)
		}
	}
	sort.Slice(genres, func(i, j int) bool {
		if count[genres[i]] != count[genres[j]] {
			return count[genres[i]] > count[genres[j]]
		}
		return genres[i] < genres[j]
	})
	maxGenres := 8
	if len(themed) > 1 {
		maxGenres = 4
	}
	for _, g := range genres[:min(maxGenres, len(genres))] {
		var items []HomeCard
		for _, c := range shuffled(movieCards) {
			if slices.Contains(c.Genres, g) && len(items) < 30 {
				items = append(items, c)
			}
		}
		keep(g, items)
	}
	home.Rows = rows

	// featured: something unwatched with a backdrop from the first row
	for _, r := range rows {
		var pick []HomeCard
		for _, c := range r.Items {
			if c.Backdrop != "" && !c.Watched {
				pick = append(pick, c)
			}
		}
		if len(pick) > 0 {
			c := pick[rnd.IntN(len(pick))]
			home.Hero = &c
			break
		}
	}
	return home
}

// season is the row for the time of year when no AI makes the rows: its
// name, the holiday (from TMDB keywords) and a genre that fill it.
func season(now time.Time) (name, holiday, genre string) {
	m, d := now.Month(), now.Day()
	switch {
	case m == time.October || m == time.November && d == 1:
		return "Halloween", "halloween", "Horror"
	case m == time.November:
		return "Thanksgiving", "thanksgiving", ""
	case m == time.December:
		return "Christmas", "christmas", ""
	case m == time.January && d <= 7:
		return "New Year", "newyear", ""
	case m == time.February && d <= 14:
		return "Valentine's Day", "valentines", "Romance"
	case m >= time.June && m <= time.August:
		return "Summer", "summer", ""
	}
	return "", "", ""
}
