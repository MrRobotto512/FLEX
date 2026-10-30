package server

import (
	"path/filepath"
	"testing"
	"time"
)

func TestContinueWatching(t *testing.T) {
	d, err := OpenDB(filepath.Join(t.TempDir(), "flex.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	exec := func(q string, args ...any) {
		if _, err := d.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	for i, title := range []string{"Heat", "Alien", "Up"} {
		exec(`INSERT INTO movies(id, path, title, runtime_s, size, mtime, added_at, seen_at) VALUES (?, ?, ?, 6000, 1, 1, ?, ?)`,
			i+1, title+".mkv", title, now, now)
	}
	exec(`INSERT INTO shows(id, key, title, added_at) VALUES (1, 'futurama', 'Futurama', ?)`, now)
	exec(`INSERT INTO shows(id, key, title, added_at) VALUES (2, 'lost', 'Lost', ?)`, now)
	for i, se := range [][2]int{{1, 1}, {1, 2}, {1, 3}} {
		exec(`INSERT INTO episodes(id, show_id, season, episode, title, path, size, mtime, added_at, seen_at)
			VALUES (?, 1, ?, ?, ?, ?, 1, 1, ?, ?)`, 10+i, se[0], se[1], "Ep", "f"+string(rune('a'+i)), now, now)
	}
	exec(`INSERT INTO episodes(id, show_id, season, episode, title, path, size, mtime, added_at, seen_at)
		VALUES (20, 2, 1, 1, 'Pilot', 'lost1', 1, 1, ?, ?)`, now, now)

	set := func(kind string, id int64, pos, dur int, ago time.Duration) {
		if err := d.SetProgress(kind, id, pos, dur); err != nil {
			t.Fatal(err)
		}
		exec(`UPDATE progress SET updated_at = ? WHERE kind = ? AND item_id = ?`,
			time.Now().Add(-ago).UTC().Format(time.RFC3339), kind, id)
	}
	set("movie", 1, 1800, 6000, 3*time.Hour)    // Heat, part way
	set("movie", 2, 5900, 6000, time.Hour)      // Alien, finished: not offered
	set("episode", 10, 1300, 1320, 2*time.Hour) // Futurama 1x01 watched -> 1x02 next
	set("episode", 20, 1320, 1320, time.Minute) // Lost finished: nothing next
	set("movie", 3, 600, 6000, 30*time.Minute)  // Up, the most recent

	items, err := d.ContinueWatching(10)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range items {
		got = append(got, it.Title+" "+it.Subtitle)
	}
	want := []string{"Up ", "Futurama S01E02 · Ep", "Heat "}
	if len(got) != len(want) {
		t.Fatalf("got %q; want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q; want %q", got, want)
		}
	}
	if items[1].Kind != "episode" || items[1].ID != 11 || items[1].PosterOf != "show" || items[1].PosterID != 1 {
		t.Errorf("next episode: %+v", items[1])
	}
	if items[2].ResumeS != 1800 {
		t.Errorf("Heat resumes at %d; want 1800", items[2].ResumeS)
	}
}
