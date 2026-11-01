package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Movies matched before FLEX kept genres get them (and keywords) from TMDB
// in the scan; keywords become holidays.
func TestFetchTags(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("append_to_response") != "keywords" {
			t.Errorf("%s without keywords", r.URL)
		}
		switch r.URL.Path {
		case "/movie/948":
			io.WriteString(w, `{"id":948,"genres":[{"id":27,"name":"Horror"}],"keywords":{"keywords":[{"name":"Halloween"},{"name":"masked killer"}]}}`)
		case "/tv/1399":
			io.WriteString(w, `{"id":1399,"genres":[{"id":18,"name":"Drama"}],"keywords":{"results":[{"name":"christmas"}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	defer func(old string) { tmdbAPI = old }(tmdbAPI)
	tmdbAPI = srv.URL

	d, err := OpenDB(filepath.Join(t.TempDir(), "flex.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	for _, q := range []string{
		`INSERT INTO movies(id, path, title, tmdb_id, size, mtime, added_at, seen_at) VALUES (1, 'a.mkv', 'Halloween', 948, 1, 1, '` + now + `', '` + now + `')`,
		`INSERT INTO movies(id, path, title, tmdb_id, size, mtime, added_at, seen_at) VALUES (2, 'b.mkv', 'Gone', 999, 1, 1, '` + now + `', '` + now + `')`,
		`INSERT INTO movies(id, path, title, size, mtime, added_at, seen_at) VALUES (3, 'c.mkv', 'Unmatched', 1, 1, '` + now + `', '` + now + `')`,
		`INSERT INTO shows(id, key, title, tmdb_id, added_at) VALUES (7, 'got', 'Game of Thrones', 1399, '` + now + `')`,
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	sc := NewScanner(Config{}, d, NewTMDB("k", t.TempDir(), quietLog()), quietLog())
	sc.fetchTags(context.Background())

	movies, _ := d.ListMovies("", "")
	got := map[string]string{}
	for _, m := range movies {
		got[m.Title] = fmt.Sprint(m.Genres, m.Holidays)
	}
	if got["Halloween"] != "[Horror] [halloween]" || got["Gone"] != "[] []" || got["Unmatched"] != "[] []" {
		t.Errorf("movies: %v", got)
	}
	shows, _ := d.ListShows("")
	if len(shows) != 1 || fmt.Sprint(shows[0].Genres, shows[0].Holidays) != "[Drama] [christmas]" {
		t.Errorf("show: %+v", shows)
	}
	if need, _ := d.TagsNeeded(); len(need) != 0 {
		t.Errorf("still to fetch: %v (a 404 must not be asked again)", need)
	}
}

// homeLibrary: Halloween (a holiday title), Heat on this server and on a
// linked one, eight more horror films and one show.
func homeLibrary() ([]Movie, []Show) {
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	movies := []Movie{
		{ID: peerStride + 3, Title: "Heat", TMDBID: 949, Genres: []string{"Crime"}, AddedAt: old.AddDate(1, 0, 0)},
		{ID: 1, Title: "Heat", TMDBID: 949, Genres: []string{"Crime"}, AddedAt: old},
		{ID: 2, Title: "Halloween", TMDBID: 948, Genres: []string{"Horror"}, Holidays: []string{"halloween"},
			Backdrop: "/api/v1/movies/2/backdrop", AddedAt: old.AddDate(2, 0, 0)},
	}
	for i := int64(0); i < 8; i++ {
		movies = append(movies, Movie{ID: 10 + i, Title: fmt.Sprintf("Horror %d", i), TMDBID: 10 + i,
			Genres: []string{"Horror"}, Watched: true, Backdrop: "x", AddedAt: old})
	}
	return movies, []Show{{ID: 7, Title: "Futurama", TMDBID: 615, AddedAt: old}}
}

func rowTitles(h Home) []string {
	var out []string
	for _, r := range h.Rows {
		out = append(out, fmt.Sprintf("%s(%d)", r.Title, len(r.Items)))
	}
	return out
}

func TestBuildHome(t *testing.T) {
	halloween := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	rnd := rand.New(rand.NewPCG(1, 2))

	movies, shows := homeLibrary()
	// a Halloween episode of Futurama, named across servers by show and number
	eps := []Episode{{ID: 70, ShowID: 7, ShowTitle: "Futurama", ShowTMDBID: 615, Season: 2, Episode: 3, Title: "Haunted House",
		Holidays: []string{"halloween"}, Poster: "/api/v1/shows/7/poster", Genres: []string{"Animation"}}}
	th := &Themes{By: "Claude", Rows: []ThemeRow{
		{"Spooky Season", []string{"m948", "e615-S02E03", "m10", "m11", "m12", "m999"}}, // m999 isn't in the library
		{"Crime Time", []string{"m949", "m10", "m11", "m12"}},
		{"Too Short", []string{"m949"}},
	}}
	h := buildHome(movies, shows, eps, nil, th, halloween, rnd)
	if got := strings.Join(rowTitles(h), " "); got != "Spooky Season(5) Recently Added(10) Crime Time(4) Horror(9)" {
		t.Errorf("rows with AI themes: %s", got)
	}
	if c := h.Rows[0].Items[1]; c.Kind != "episode" || c.ID != 70 || c.Subtitle != "S02E03 · Haunted House" || c.ShowID != 7 || c.Title != "Futurama" {
		t.Errorf("episode card: %+v", c)
	}
	if h.Themes != "Claude" || h.Continue == nil {
		t.Errorf("themes %q, continue %v", h.Themes, h.Continue)
	}
	if c := h.Rows[2].Items[0]; c.ID != 1 || c.Kind != "movie" {
		t.Errorf("Heat on both servers must be this server's: %+v", c)
	}
	for _, c := range h.Rows[1].Items {
		if c.Title == "Heat" && c.ID != 1 {
			t.Errorf("Recently Added has the linked Heat: %+v", c)
		}
	}
	if h.Hero == nil || h.Hero.ID != 2 {
		t.Errorf("hero must be the unwatched title with a backdrop in the first row: %+v", h.Hero)
	}

	// no AI: a Halloween row from keywords, then horror
	movies, shows = homeLibrary()
	h = buildHome(movies, shows, eps, nil, nil, halloween, rnd)
	if got := strings.Join(rowTitles(h), " "); got != "Halloween(10) Recently Added(10) Horror(9)" {
		t.Errorf("rows from TMDB: %s", got)
	}
	if !slices.ContainsFunc(h.Rows[0].Items, func(c HomeCard) bool { return c.Kind == "episode" && c.ID == 70 }) {
		t.Error("the Halloween episode belongs in the TMDB Halloween row")
	}
	if h.Themes != "TMDB" || h.Rows[0].Items[0].ID != 2 {
		t.Errorf("the holiday title leads the Halloween row: %s %+v", h.Themes, h.Rows[0].Items[0])
	}
	// same shuffle all day
	movies, shows = homeLibrary()
	again := buildHome(movies, shows, eps, nil, nil, halloween.Add(time.Hour), rnd)
	if fmt.Sprint(again.Rows[2].Items) != fmt.Sprint(h.Rows[2].Items) {
		t.Error("genre rows must keep their order through the day")
	}
}

type fakeAI struct {
	prompt string
	answer string
}

func (f *fakeAI) name() string { return "Fake" }
func (f *fakeAI) ask(_ context.Context, system, prompt string, schema map[string]any) (string, error) {
	f.prompt = prompt
	return f.answer, nil
}

func TestThemerMake(t *testing.T) {
	ai := &fakeAI{answer: `{"rows":[
		{"title":"Mind Benders","items":["m603","m604","m605","m606","m999","m603"]},
		{"title":"Too Few","items":["m603","m604"]},
		{"title":"  ","items":["m603","m604","m605","m606"]}]}`}
	tm := &themer{s: &Server{cfg: Config{Name: "Robot"}}, ai: ai}
	catalog := themeCatalog(nil, nil, []Episode{{ShowTitle: "The Office", ShowTMDBID: 2316, Season: 2, Episode: 10, Title: "Christmas Party", Genres: []string{"Comedy"}}})
	if len(catalog) != 1 || catalog[0].id != "e2316-S02E10" || catalog[0].name != `The Office S02E10 "Christmas Party" (episode)` {
		t.Errorf("episode in the catalog: %+v", catalog)
	}
	catalog = nil
	for i := 603; i <= 606; i++ {
		catalog = append(catalog, themeTitle{fmt.Sprintf("m%d", i), fmt.Sprintf("Matrix %d (1999)", i), []string{"Action", "Science Fiction"}})
	}
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	th, err := tm.make(context.Background(), catalog, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(th.Rows) != 1 || th.Rows[0].Title != "Mind Benders" || len(th.Rows[0].Items) != 4 || th.By != "Fake" ||
		th.From != "Robot" || th.Titles != 4 || !th.Made.Equal(now) {
		t.Errorf("themes: %+v", th)
	}
	if !strings.Contains(ai.prompt, "Saturday, 31 October 2026") || !strings.Contains(ai.prompt, "m603 | Matrix 603 (1999) | Action, Science Fiction\n") {
		t.Errorf("prompt:\n%s", ai.prompt)
	}

	made := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC) // a Monday
	cur := &Themes{Made: made, Titles: 100}
	for _, c := range []struct {
		now    time.Time
		titles int
		stale  bool
	}{
		{made.AddDate(0, 0, 5), 100, false},
		{made.AddDate(0, 0, 6), 100, true},  // Sunday 1 November: same week, new month
		{made.AddDate(0, 0, 7), 100, true},  // next week
		{made.AddDate(0, 0, 1), 126, true},  // a quarter more titles
		{made.AddDate(0, 0, 1), 125, false}, // not quite
	} {
		if got := cur.stale(c.now, c.titles); got != c.stale {
			t.Errorf("stale(%s, %d) = %v", c.now.Format("Mon 2 Jan"), c.titles, got)
		}
	}
}

// A server without a key shows a linked server's AI rows.
func TestThemesBorrowed(t *testing.T) {
	d, err := OpenDB(filepath.Join(t.TempDir(), "flex.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ps := httptest.NewServer(&fakePeer{})
	defer ps.Close()
	s := &Server{cfg: Config{}, db: d, id: "front", log: quietLog()}
	s.peers = newPeers(s)
	s.peers.check(context.Background(), []string{strings.TrimPrefix(ps.URL, "http://")})
	s.themes = newThemer(s)
	s.themes.borrow(context.Background(), nil)
	if th := s.themes.current(); th == nil || th.From != "Garage" || th.Rows[0].Items[0] != "m603" {
		t.Fatalf("borrowed: %+v", th)
	}
	if st := s.themes.Status(); st != "Rows by Garage's Claude, made 26 Oct" {
		t.Errorf("status %q", st)
	}
	// kept across restarts
	if th := newThemer(s).current(); th == nil || th.From != "Garage" {
		t.Errorf("not saved: %+v", th)
	}
}

// The three providers' requests and answers, against fakes of their APIs.
func TestAIProviders(t *testing.T) {
	schema := themesSchema
	var got map[string]any
	var hdr http.Header
	var path string
	fake := func(answer func(w http.ResponseWriter)) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path, hdr, got = r.URL.Path, r.Header.Clone(), nil
			json.NewDecoder(r.Body).Decode(&got)
			answer(w)
		}))
	}
	field := func(m map[string]any, keys ...string) any {
		var v any = m
		for _, k := range keys {
			mm, _ := v.(map[string]any)
			v = mm[k]
		}
		return v
	}

	t.Run("claude", func(t *testing.T) {
		srv := fake(func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, ev := range []string{
				`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"{\"rows\":"}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"[]}"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`,
				`{"type":"message_stop"}`,
			} {
				var e struct{ Type string }
				json.Unmarshal([]byte(ev), &e)
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, ev)
			}
		})
		defer srv.Close()
		defer func(old string) { anthropicAPI = old }(anthropicAPI)
		anthropicAPI = srv.URL
		out, err := newClaude("sk-test", "").ask(context.Background(), "sys", "prompt", schema)
		if err != nil || out != `{"rows":[]}` {
			t.Fatalf("answer %q, %v", out, err)
		}
		if path != "/v1/messages" || hdr.Get("X-Api-Key") != "sk-test" || !strings.Contains(hdr.Get("Anthropic-Beta"), "server-side-fallback-2026-07-01") {
			t.Errorf("request: %s %v", path, hdr)
		}
		if got["model"] != "claude-opus-5-5" || got["fallbacks"] != "default" || got["stream"] != true ||
			field(got, "output_config", "format", "type") != "json_schema" || field(got, "output_config", "effort") != "medium" ||
			field(got, "output_config", "format", "schema", "required") == nil {
			t.Errorf("body: %v", got)
		}
	})

	t.Run("chatgpt", func(t *testing.T) {
		reply := `{"status":"completed","output":[{"type":"reasoning","content":[]},
			{"type":"message","content":[{"type":"output_text","text":"{\"rows\":[]}"}]}]}`
		srv := fake(func(w http.ResponseWriter) { io.WriteString(w, reply) })
		defer srv.Close()
		defer func(old string) { openaiAPI = old }(openaiAPI)
		openaiAPI = srv.URL
		ai, _ := newAsker(Config{AIProvider: "openai", AIKey: "sk-o"})
		out, err := ai.ask(context.Background(), "sys", "prompt", schema)
		if err != nil || out != `{"rows":[]}` || ai.name() != "ChatGPT" {
			t.Fatalf("answer %q, %v", out, err)
		}
		if path != "/responses" || hdr.Get("Authorization") != "Bearer sk-o" || got["model"] != openaiModel ||
			got["instructions"] != "sys" || got["input"] != "prompt" || field(got, "text", "format", "type") != "json_schema" ||
			field(got, "text", "format", "strict") != true || field(got, "text", "format", "schema", "additionalProperties") != false {
			t.Errorf("request %s %v", path, got)
		}
		reply = `{"status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"no"}]}]}`
		if _, err := ai.ask(context.Background(), "sys", "prompt", schema); err == nil || !strings.Contains(err.Error(), "declined") {
			t.Errorf("refusal: %v", err)
		}
		reply = `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`
		if _, err := ai.ask(context.Background(), "sys", "prompt", schema); err == nil || !strings.Contains(err.Error(), "cut off") {
			t.Errorf("incomplete: %v", err)
		}
	})

	t.Run("gemini", func(t *testing.T) {
		code, reply := 200, `{"candidates":[{"content":{"parts":[{"text":"thinking…","thought":true},{"text":"{\"rows\":[]}"}]},"finishReason":"STOP"}]}`
		srv := fake(func(w http.ResponseWriter) { w.WriteHeader(code); io.WriteString(w, reply) })
		defer srv.Close()
		defer func(old string) { geminiAPI = old }(geminiAPI)
		geminiAPI = srv.URL
		ai, _ := newAsker(Config{AIProvider: "gemini", AIKey: "g-key", AIModel: "gemini-x"})
		out, err := ai.ask(context.Background(), "sys", "prompt", schema)
		if err != nil || out != `{"rows":[]}` {
			t.Fatalf("answer %q, %v", out, err)
		}
		raw, _ := json.Marshal(got)
		if path != "/models/gemini-x:generateContent" || hdr.Get("X-Goog-Api-Key") != "g-key" ||
			field(got, "generationConfig", "responseMimeType") != "application/json" ||
			field(got, "generationConfig", "responseSchema", "type") != "OBJECT" ||
			strings.Contains(string(raw), "additionalProperties") || !strings.Contains(string(raw), `"text":"sys"`) {
			t.Errorf("request %s %s", path, raw)
		}
		code, reply = 400, `{"error":{"code":400,"message":"API key not valid."}}`
		if _, err := ai.ask(context.Background(), "sys", "prompt", schema); err == nil || err.Error() != "HTTP 400: API key not valid." {
			t.Errorf("bad key: %v", err)
		}
		code, reply = 200, `{"promptFeedback":{"blockReason":"SAFETY"}}`
		if _, err := ai.ask(context.Background(), "sys", "prompt", schema); err == nil || !strings.Contains(err.Error(), "SAFETY") {
			t.Errorf("blocked: %v", err)
		}
	})

	if ai, err := newAsker(Config{AIKey: "k", AIProvider: "llama"}); ai != nil || err == nil {
		t.Error("an unknown provider must be an error")
	}
	if ai, _ := newAsker(Config{AIProvider: "claude"}); ai != nil {
		t.Error("no key, no AI")
	}
}
