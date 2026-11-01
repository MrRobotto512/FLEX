package server

import (
	"path/filepath"
	"testing"
	"time"
)

// The AI's seasonal row picks this server's SD titles; what it drops is
// removed; what the owner added stays; HD and linked titles are left alone.
func TestPrepPick(t *testing.T) {
	d, err := OpenDB(filepath.Join(t.TempDir(), "flex.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	sd := `{"video":{"codec":"mpeg4","width":640,"height":480},"duration_s":100}`
	hd := `{"video":{"codec":"h264","width":1920,"height":1080},"duration_s":100}`
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO movies(id, path, title, tmdb_id, media_json, size, mtime, added_at, seen_at) VALUES (1, 'h.avi', 'Halloween', 948, ?, 1, 1, ?, ?)`, []any{sd, now, now}},
		{`INSERT INTO movies(id, path, title, tmdb_id, media_json, size, mtime, added_at, seen_at) VALUES (2, 'g.mkv', 'Ghostbusters', 620, ?, 1, 1, ?, ?)`, []any{hd, now, now}},
		{`INSERT INTO movies(id, path, title, tmdb_id, media_json, size, mtime, added_at, seen_at) VALUES (3, 'c.avi', 'Casper', 8839, ?, 1, 1, ?, ?)`, []any{sd, now, now}},
		{`INSERT INTO shows(id, key, title, tmdb_id, added_at) VALUES (7, 'f', 'Futurama', 615, ?)`, []any{now}},
		{`INSERT INTO episodes(id, show_id, season, episode, title, path, media_json, size, mtime, added_at, seen_at) VALUES (70, 7, 2, 3, 'Haunted', 'f.avi', ?, 1, 1, ?, ?)`, []any{sd, now, now}},
	} {
		if _, err := d.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{cfg: Config{PrepEnabled: true, PrepMax: 12}, db: d, log: quietLog()}
	s.tc = NewTranscoder("", "libx264", t.TempDir(), s.log)
	s.themes = &themer{s: s, cur: &Themes{Rows: []ThemeRow{{"Spooky Season", []string{"m948", "m620", "e615-S02E03", "m100000"}}}}}
	p := newPrepper(s)

	// the season's row: Halloween (SD) and the episode; Ghostbusters is HD, m100000 unknown
	p.pick()
	list := p.List()
	if len(list) != 2 || list[0].Kind != "movie" || list[0].ID != 1 || list[1].Kind != "episode" || list[1].ID != 70 ||
		list[0].Source != "ai" || list[0].State != prepStateQueued {
		t.Fatalf("picked: %+v", list)
	}
	if err := p.Add("movie", 3); err != nil {
		t.Fatal(err)
	}
	if err := p.Add("movie", 2); err == nil {
		t.Error("an HD title can't be prepared")
	}
	if err := p.Add("movie", peerStride+1); err == nil {
		t.Error("a linked server's title can't be prepared here")
	}

	// the season moves on: the AI's row no longer lists Halloween; Casper (owner's) stays
	s.themes.cur = &Themes{Rows: []ThemeRow{{"Cozy Winter", []string{"e615-S02E03"}}}}
	p.pick()
	var got []string
	for _, it := range p.List() {
		got = append(got, it.Kind+":"+it.Title)
	}
	if len(got) != 2 || got[0] != "episode:Futurama S02E03 Haunted" || got[1] != "movie:Casper" {
		t.Errorf("after the season: %v", got)
	}
	p.Remove("movie", 3)
	if n := len(p.List()); n != 1 {
		t.Errorf("after removing Casper: %d left", n)
	}
	if key := d.PrepKey("episode", 70); key != "e615-S02E03" {
		t.Errorf("episode key %q", key)
	}
	if p.Ready("movie", 1) != "" {
		t.Error("nothing is ready yet")
	}
}
