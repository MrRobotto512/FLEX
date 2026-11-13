package main

// FLEX is a windowed program: it has no console, so what it wrote to stderr
// (its log, and the Go runtime's report when it dies of a panic or fatal
// error) went nowhere, and a FLEX that closed by itself left no trace. This
// keeps all of it in %APPDATA%\FLEX\log\flex.log, with the two runs before
// as flex.1.log and flex.2.log: the end of the file says how the run ended
// ("closing: ..." for an orderly exit, a Go traceback for a crash, nothing at
// all if the process was killed from outside).

import (
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"flex/server"
)

var logFile *os.File // this run's log, nil when it couldn't be made

func logDir() string { return filepath.Join(filepath.Dir(settingsPath()), "log") }

// openLog starts this run's log file and returns the logger writing to it
// (stderr if the file can't be made).
func openLog() *slog.Logger {
	dir := logDir()
	os.MkdirAll(dir, 0o755)
	name := filepath.Join(dir, "flex.log")
	os.Remove(filepath.Join(dir, "flex.2.log"))
	os.Rename(filepath.Join(dir, "flex.1.log"), filepath.Join(dir, "flex.2.log"))
	os.Rename(name, filepath.Join(dir, "flex.1.log"))
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	logFile = f
	debug.SetCrashOutput(f, debug.CrashOptions{}) // panics and fatal errors, with every goroutine
	debug.SetTraceback("all")
	os.Stderr = f // libraries that print there
	log.SetOutput(f)
	l := slog.New(slog.NewTextHandler(&capped{f: f}, nil))
	l.Info("FLEX started", "version", server.Version, "pid", os.Getpid(), "args", strings.Join(os.Args[1:], " "))
	go heartbeat(l)
	return l
}

// heartbeat leaves a line every five minutes, so a run that ends without a
// word still shows when it stopped and how much memory it held.
func heartbeat(l *slog.Logger) {
	start := time.Now()
	for range time.Tick(5 * time.Minute) {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		l.Info("still running", "up", time.Since(start).Round(time.Minute).String(),
			"goroutines", runtime.NumGoroutine(), "heap_mb", m.HeapAlloc>>20, "sys_mb", m.Sys>>20)
	}
}

// capped stops the ordinary log at 64 MB a run (a crash report still goes
// to the file directly), so a server that logs in a loop can't fill the disk.
type capped struct {
	f    *os.File
	n    int64
	full bool
}

func (c *capped) Write(p []byte) (int, error) {
	if c.full {
		return len(p), nil
	}
	if c.n += int64(len(p)); c.n > 64<<20 {
		c.full = true
		fmt.Fprintln(c.f, "log: 64 MB written this run, the rest is left out")
		return len(p), nil
	}
	return c.f.Write(p)
}

var _ io.Writer = (*capped)(nil)
