package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// An AI (Claude, ChatGPT or Gemini, with the user's own key) arranges the
// whole library, linked servers' titles included, into the home screen's
// themed rows ("Halloween night", "80s action", …), about once a week. The
// rows name titles by TMDB id, so they work on every linked server: one
// without a key of its own borrows a linked server's rows.

const (
	themesSetting = "home_themes" // settings row holding the current Themes
	themesCheck   = 30 * time.Minute
	themesRetry   = 6 * time.Hour // between requests to the AI, whatever came of the last one
)

// ThemeRow is one row the AI made. Items are "m<tmdb id>" for movies and
// "s<tmdb id>" for shows.
type ThemeRow struct {
	Title string   `json:"title"`
	Items []string `json:"items"`
}

// Themes are the rows and where they came from.
type Themes struct {
	By     string     `json:"by"`   // "Claude", "ChatGPT" or "Gemini"
	From   string     `json:"from"` // the server that asked it
	Made   time.Time  `json:"made"`
	Titles int        `json:"titles"` // how many titles the library had
	Rows   []ThemeRow `json:"rows"`
}

type themer struct {
	s  *Server
	ai asker // nil: borrow from linked servers

	mu    sync.Mutex
	cur   *Themes
	err   string
	tried time.Time
}

func newThemer(s *Server) *themer {
	t := &themer{s: s}
	ai, err := newAsker(s.cfg)
	if err != nil {
		t.err = err.Error()
	}
	t.ai = ai
	if raw, _ := s.db.GetSetting(themesSetting); raw != "" {
		var th Themes
		if json.Unmarshal([]byte(raw), &th) == nil {
			t.cur = &th
		}
	}
	return t
}

func (t *themer) current() *Themes {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cur
}

func (t *themer) set(th *Themes) {
	raw, _ := json.Marshal(th)
	t.s.db.SetSetting(themesSetting, string(raw))
	t.mu.Lock()
	t.cur, t.err = th, ""
	t.mu.Unlock()
}

// run looks every half hour whether the rows are due, after giving linked
// servers time to turn up and the start-up scan time to finish.
func (t *themer) run(ctx context.Context) {
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		t.refresh(ctx, time.Now())
		timer.Reset(themesCheck)
	}
}

// stale: the rows are made again each week, when the month turns (another
// holiday) and when the library has grown by a quarter.
func (th *Themes) stale(now time.Time, titles int) bool {
	y1, w1 := th.Made.ISOWeek()
	y2, w2 := now.ISOWeek()
	return y1 != y2 || w1 != w2 || th.Made.Month() != now.Month() || titles > th.Titles*5/4
}

func (t *themer) refresh(ctx context.Context, now time.Time) {
	if t.s.scanner != nil && t.s.scanner.Status().Scanning {
		return
	}
	catalog := themeCatalog(t.s.library(ctx))
	if len(catalog) < 20 {
		return // too little to arrange
	}
	cur := t.current()
	if cur != nil && !cur.stale(now, len(catalog)) {
		return
	}
	if t.ai == nil {
		t.borrow(ctx, cur)
		return
	}
	t.mu.Lock()
	if !t.tried.IsZero() && now.Sub(t.tried) < themesRetry {
		t.mu.Unlock()
		return
	}
	t.tried = now
	t.mu.Unlock()

	th, err := t.make(ctx, catalog, now)
	if err != nil {
		t.s.log.Warn("home rows", "ai", t.ai.name(), "err", err)
		t.mu.Lock()
		t.err = err.Error()
		t.mu.Unlock()
		return
	}
	t.s.log.Info("home rows made", "ai", t.ai.name(), "rows", len(th.Rows), "titles", th.Titles)
	t.set(th)
}

// borrow takes the newest rows a linked server's AI made.
func (t *themer) borrow(ctx context.Context, cur *Themes) {
	if t.s.peers == nil {
		return
	}
	best := cur
	for _, pe := range t.s.peers.online() {
		var th Themes
		if err := t.s.peers.getJSON(ctx, pe, "/api/v1/home/themes", &th); err != nil || len(th.Rows) == 0 {
			continue
		}
		if best == nil || th.Made.After(best.Made) {
			best = &th
		}
	}
	if best != nil && best != cur {
		t.set(best)
	}
}

// themeTitle is one line of the library as the AI sees it.
type themeTitle struct {
	id     string // "m603", "s1399"
	name   string // "The Matrix (1999)"
	genres []string
}

// themeCatalog lists every matched title once, however many servers have it.
func themeCatalog(movies []Movie, shows []Show) []themeTitle {
	var out []themeTitle
	seen := map[string]bool{}
	add := func(id, title string, year int, genres []string) {
		if seen[id] {
			return
		}
		seen[id] = true
		name := title
		if year != 0 {
			name = fmt.Sprintf("%s (%d)", title, year)
		}
		out = append(out, themeTitle{id, name, genres})
	}
	for _, m := range movies {
		if m.TMDBID != 0 {
			add(fmt.Sprintf("m%d", m.TMDBID), m.Title, m.Year, m.Genres)
		}
	}
	for _, sh := range shows {
		if sh.TMDBID != 0 {
			add(fmt.Sprintf("s%d", sh.TMDBID), sh.Title+" (TV series)", sh.Year, sh.Genres)
		}
	}
	return out
}

const themesSystem = `You arrange the home screen of a family's home media server into rows, the way Netflix does. The library comes one title per line: id | title (year) | genres.`

const themesPrompt = `Today is %s.

Make 10 to 14 rows from this library. Open with one or two rows for the time of year (a holiday that is on or coming up, or the season) when the library has enough titles that fit; make the rest varied and fun to browse: moods, decades, franchises, directors or stars, genre blends, overlooked gems. Each row has 8 to 30 titles in the order they should be shown and a short, catchy name (at most 32 characters, no emoji). A title can be in up to three rows. Use only ids from the library.

<library>
%s</library>`

var themesSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"rows": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"title": map[string]any{"type": "string"},
					"items": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
				"required":             []any{"title", "items"},
				"additionalProperties": false,
			},
		},
	},
	"required":             []any{"rows"},
	"additionalProperties": false,
}

// make asks the AI for the rows and keeps what holds up: known ids, rows of
// at least four.
func (t *themer) make(ctx context.Context, catalog []themeTitle, now time.Time) (*Themes, error) {
	var lib strings.Builder
	known := map[string]bool{}
	for _, c := range catalog {
		fmt.Fprintf(&lib, "%s | %s | %s\n", c.id, c.name, strings.Join(c.genres, ", "))
		known[c.id] = true
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	answer, err := t.ai.ask(ctx, themesSystem, fmt.Sprintf(themesPrompt, now.Format("Monday, 2 January 2006"), lib.String()), themesSchema)
	if err != nil {
		return nil, err
	}
	var out struct {
		Rows []ThemeRow `json:"rows"`
	}
	if err := json.Unmarshal([]byte(answer), &out); err != nil {
		return nil, fmt.Errorf("%s's answer isn't the JSON asked for: %w", t.ai.name(), err)
	}
	th := &Themes{By: t.ai.name(), From: t.s.cfg.Name, Made: now, Titles: len(catalog)}
	for _, r := range out.Rows {
		var items []string
		in := map[string]bool{}
		for _, id := range r.Items {
			if known[id] && !in[id] && len(items) < 30 {
				in[id] = true
				items = append(items, id)
			}
		}
		title := strings.TrimSpace(r.Title)
		if len(items) < 4 || title == "" {
			continue
		}
		if utf8.RuneCountInString(title) > 40 {
			title = string([]rune(title)[:40])
		}
		th.Rows = append(th.Rows, ThemeRow{title, items})
		if len(th.Rows) == 16 {
			break
		}
	}
	if len(th.Rows) == 0 {
		return nil, errors.New(t.ai.name() + " made no usable rows")
	}
	return th, nil
}

// Status says who makes the home screen's rows, for the desktop window.
func (t *themer) Status() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case t.err != "" && t.ai != nil:
		return t.ai.name() + ": " + t.err
	case t.err != "":
		return t.err
	case t.cur != nil && t.ai == nil:
		return fmt.Sprintf("Rows by %s's %s, made %s", t.cur.From, t.cur.By, t.cur.Made.Format("2 Jan"))
	case t.cur != nil:
		return fmt.Sprintf("Rows by %s, made %s", t.cur.By, t.cur.Made.Format("2 Jan"))
	case t.ai != nil:
		return t.ai.name() + " makes the rows once the library scan is done"
	}
	return "Rows from TMDB genres (choose an AI for themed rows)"
}
