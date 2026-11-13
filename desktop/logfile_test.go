package main

import (
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

func TestLogFile(t *testing.T) {
	t.Setenv("APPDATA", t.TempDir())
	stderr := os.Stderr
	defer func() {
		os.Stderr = stderr
		log.SetOutput(stderr)
		debug.SetCrashOutput(nil, debug.CrashOptions{})
		logFile.Close()
	}()
	openLog().Info("first run")
	debug.SetCrashOutput(nil, debug.CrashOptions{})
	logFile.Close() // as when the run before has ended
	l := openLog()
	l.Info("closing: test")
	log.Print("from a library")
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(logDir(), name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if s := read("flex.1.log"); !strings.Contains(s, "first run") || !strings.Contains(s, "FLEX started") {
		t.Errorf("the run before: %q", s)
	}
	if s := read("flex.log"); !strings.Contains(s, "closing: test") || !strings.Contains(s, "from a library") || strings.Contains(s, "first run") {
		t.Errorf("this run: %q", s)
	}
}
