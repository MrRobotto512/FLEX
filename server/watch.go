package server

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The library keeps itself up to date. Every library folder is watched (on
// Windows with the system's change notifications, so idle drives can spin
// down; elsewhere by a light look every few minutes) and once changes have
// been quiet for a while a scan runs, so a movie still being copied isn't
// read half written. A nightly scan catches what notifications miss, such
// as some network drives.

var (
	watchSettle  = 30 * time.Second // quiet after the last change before scanning
	watchRetry   = 5 * time.Minute  // before watching a folder again after an error (drive unplugged)
	nightlyHour  = 4                // local hour of the nightly scan
	watchStarted func(dir string)   // tests: called once a folder is being watched
)

// watchLibrary runs until ctx ends. scan starts a scan and says false when
// one is already running; the desktop app restarts the server (and so this)
// when the folder list changes.
func watchLibrary(ctx context.Context, roots []Root, scan func() bool, log *slog.Logger) {
	changed := make(chan string, 1)
	for _, r := range roots {
		go keepWatching(ctx, r.Path, func() {
			select {
			case changed <- r.Path:
			default:
			}
		}, log)
	}
	settle := time.NewTimer(time.Hour)
	settle.Stop()
	nightly := time.NewTimer(untilHour(time.Now(), nightlyHour))
	defer settle.Stop()
	defer nightly.Stop()
	pending := ""
	for {
		select {
		case <-ctx.Done():
			return
		case dir := <-changed:
			pending = dir
			settle.Reset(watchSettle)
		case <-settle.C:
			if pending == "" {
				continue
			}
			if !scan() {
				settle.Reset(watchSettle) // a scan is running, which may have missed it: again after it
				continue
			}
			log.Info("library: files changed, scanning", "folder", pending)
			pending = ""
		case <-nightly.C:
			log.Info("library: nightly scan")
			scan()
			nightly.Reset(untilHour(time.Now(), nightlyHour))
		}
	}
}

// keepWatching watches one folder, again after errors. A folder that comes
// back (a drive plugged in again) counts as changed.
func keepWatching(ctx context.Context, dir string, changed func(), log *slog.Logger) {
	failed := false
	for ctx.Err() == nil {
		err := watchFolder(ctx, dir, changed, func() {
			if failed {
				changed()
				failed = false
			}
			if watchStarted != nil {
				watchStarted(dir)
			}
		})
		if ctx.Err() != nil {
			return
		}
		if !failed {
			log.Warn("library: can't watch folder, trying again later", "folder", dir, "err", err)
		}
		failed = true
		select {
		case <-ctx.Done():
			return
		case <-time.After(watchRetry):
		}
	}
}

// otherFiles sit next to movies; changes to them don't change the library.
var otherFiles = map[string]bool{
	".nfo": true, ".jpg": true, ".jpeg": true, ".png": true, ".txt": true, ".srt": true, ".sub": true,
	".idx": true, ".ass": true, ".ssa": true, ".vtt": true, ".part": true, ".tmp": true, ".crdownload": true,
	".db": true, ".ini": true, ".url": true, ".log": true, ".xml": true, ".json": true, ".!qb": true,
}

// relevantChange says whether a change at path can change the library: a
// video file, a folder coming in (moved in whole, its movies bring no
// changes of their own) or anything going away that may have been a folder.
func relevantChange(path string, added, removed bool) bool {
	ext := strings.ToLower(filepath.Ext(path))
	if videoExt[ext] {
		return true
	}
	switch {
	case added:
		fi, err := os.Stat(path)
		return err == nil && fi.IsDir()
	case removed:
		return !otherFiles[ext]
	}
	return false
}

// untilHour is the time until the next hour:00, local time.
func untilHour(now time.Time, hour int) time.Duration {
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next.Sub(now)
}
