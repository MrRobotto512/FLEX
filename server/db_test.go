package server

import (
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDeleteOutside(t *testing.T) {
	d, err := OpenDB(filepath.Join(t.TempDir(), "flex.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	base := t.TempDir()
	movies, movies2, tv := filepath.Join(base, "Movies"), filepath.Join(base, "Movies 2"), filepath.Join(base, "TV")
	now := time.Now().UTC().Format(time.RFC3339)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO movies(id, path, title, size, mtime, added_at, seen_at) VALUES (1, ?, 'Heat', 1, 1, ?, ?)`,
			[]any{filepath.Join(movies, "Heat.mkv"), now, now}},
		{`INSERT INTO movies(id, path, title, size, mtime, added_at, seen_at) VALUES (2, ?, 'Alien', 1, 1, ?, ?)`,
			[]any{filepath.Join(movies2, "Alien", "Alien.mkv"), now, now}},
		{`INSERT INTO shows(id, key, title, added_at) VALUES (7, 'futurama', 'Futurama', ?)`, []any{now}},
		{`INSERT INTO episodes(id, show_id, season, episode, title, path, size, mtime, added_at, seen_at)
			VALUES (70, 7, 1, 1, 'Space Pilot 3000', ?, 1, 1, ?, ?)`,
			[]any{filepath.Join(tv, "Futurama", "S01E01.mkv"), now, now}},
		{`INSERT INTO progress(kind, item_id, position_s, duration_s, updated_at) VALUES ('movie', 2, 60, 100, ?), ('episode', 70, 60, 100, ?)`,
			[]any{now, now}},
	} {
		if _, err := d.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	count := func(table string) (n int) {
		d.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n)
		return n
	}

	// "Movies 2" goes, with Alien's progress; Heat in "Movies" (a prefix of
	// "Movies 2") stays, whatever the case or a trailing slash say
	sep := string(filepath.Separator)
	if n, err := d.DeleteOutside([]string{strings.ToUpper(movies) + sep, tv}); err != nil || n != 1 {
		t.Fatalf("removing Movies 2: %d, %v", n, err)
	}
	var id int64
	d.QueryRow(`SELECT id FROM movies`).Scan(&id)
	if count("movies") != 1 || id != 1 || count("episodes") != 1 || count("progress") != 1 {
		t.Errorf("after removing Movies 2: movies %d (id %d), episodes %d, progress %d",
			count("movies"), id, count("episodes"), count("progress"))
	}

	// the TV folder goes: its episodes, then the show and the episode's progress
	if n, err := d.DeleteOutside([]string{movies}); err != nil || n != 1 {
		t.Fatalf("removing TV: %d, %v", n, err)
	}
	if count("movies") != 1 || count("episodes") != 0 || count("shows") != 0 || count("progress") != 0 {
		t.Errorf("after removing TV: movies %d, episodes %d, shows %d, progress %d",
			count("movies"), count("episodes"), count("shows"), count("progress"))
	}

	// the last folder going through the API leaves nothing
	s := &Server{cfg: Config{}, db: d, log: slog.New(slog.NewTextHandler(io.Discard, nil)), folders: []string{movies}}
	if err := s.SetFolders(nil); err != nil {
		t.Fatal(err)
	}
	if count("movies") != 0 {
		t.Errorf("no folders left, still %d movies", count("movies"))
	}
}
