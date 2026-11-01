package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRelevantChange(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "Heat.1995.1080p"), 0o755)
	for _, c := range []struct {
		path           string
		added, removed bool
		want           bool
	}{
		{"Heat.mkv", false, false, true},           // a video written to
		{"Heat.nfo", true, false, false},           // a side file
		{"Heat.1995.1080p", true, false, true},     // a folder moved in (it exists)
		{"notes.txt", true, false, false},          // a file that isn't a video
		{"Alien.1979.BluRay", false, true, true},   // gone: may have been a movie's folder
		{"poster.jpg", false, true, false},         // a side file gone
		{"Heat.1995.1080p", false, false, false},   // a folder's contents changing: its videos say so
		{"Gone.2001.mkv.part", true, false, false}, // a download in progress
		{"Gone.2001.mkv", true, false, true},       // the finished download, renamed
	} {
		if got := relevantChange(filepath.Join(dir, c.path), c.added, c.removed); got != c.want {
			t.Errorf("relevantChange(%s, added %v, removed %v) = %v", c.path, c.added, c.removed, got)
		}
	}
}

func TestUntilHour(t *testing.T) {
	// UTC: in a zone where the clocks change tonight 22:00 → 04:00 really is 7 hours
	at := func(h, m int) time.Time { return time.Date(2026, 10, 31, h, m, 0, 0, time.UTC) }
	if d := untilHour(at(3, 30), 4); d != 30*time.Minute {
		t.Errorf("03:30 → %v", d)
	}
	if d := untilHour(at(4, 0), 4); d != 24*time.Hour {
		t.Errorf("04:00 → %v", d)
	}
	if d := untilHour(at(22, 0), 4); d != 6*time.Hour {
		t.Errorf("22:00 → %v", d)
	}
}

// New, removed and moved-in movies start a scan once things settle; other
// files don't; a scan already running means another one after it.
func TestWatchLibrary(t *testing.T) {
	defer func(s time.Duration, f func(string)) { watchSettle, watchStarted = s, f }(watchSettle, watchStarted)
	watchSettle = 300 * time.Millisecond
	setWatchPollForTest(t, 100*time.Millisecond)
	root := t.TempDir()
	ready := make(chan string, 1)
	watchStarted = func(dir string) { ready <- dir }

	scans := make(chan bool, 10)
	busy := false
	scan := func() bool {
		if busy {
			busy = false
			scans <- false
			return false
		}
		scans <- true
		return true
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchLibrary(ctx, []Root{{Path: root}}, scan, quietLog())
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("the folder was never watched")
	}

	expect := func(what string, want ...bool) {
		t.Helper()
		for _, w := range want {
			select {
			case got := <-scans:
				if got != w {
					t.Fatalf("%s: scan call said %v, want %v", what, got, w)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%s: no scan", what)
			}
		}
	}
	quiet := func(what string) {
		t.Helper()
		select {
		case <-scans:
			t.Fatalf("%s: scanned", what)
		case <-time.After(time.Second):
		}
	}

	os.MkdirAll(filepath.Join(root, "Heat (1995)"), 0o755)
	os.WriteFile(filepath.Join(root, "Heat (1995)", "Heat.mkv"), []byte("x"), 0o644)
	expect("new movie", true)

	os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0o644)
	quiet("a text file")

	busy = true // the next scan call finds one running: it is tried again
	os.Remove(filepath.Join(root, "Heat (1995)", "Heat.mkv"))
	expect("removed movie", false, true)

	// a whole movie folder moved in from elsewhere on the drive
	other := t.TempDir()
	os.MkdirAll(filepath.Join(other, "Alien (1979)"), 0o755)
	os.WriteFile(filepath.Join(other, "Alien (1979)", "Alien.mkv"), []byte("x"), 0o644)
	if err := os.Rename(filepath.Join(other, "Alien (1979)"), filepath.Join(root, "Alien (1979)")); err != nil {
		t.Skip("can't move between temp folders here:", err)
	}
	expect("movie folder moved in", true)
}
