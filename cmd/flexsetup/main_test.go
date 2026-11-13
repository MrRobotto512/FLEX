//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReplaceFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "FLEX.exe")
	for _, body := range []string{"one", "two"} {
		if err := replaceFile(p, []byte(body)); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(p); string(b) != body {
			t.Fatalf("got %q, want %q", b, body)
		}
	}
}

func TestShortcut(t *testing.T) {
	dir := t.TempDir()
	lnk := filepath.Join(dir, "it's a test", "FLEX.lnk") // a quote in the path must not matter
	os.MkdirAll(filepath.Dir(lnk), 0o755)
	if err := makeShortcut(lnk, filepath.Join(dir, "FLEX.exe"), dir); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveDirOnlyFLEX(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Other")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "FLEX.exe"), []byte("x"), 0o644)
	removeDir(dir)
	if !exists(filepath.Join(dir, "FLEX.exe")) {
		t.Error("removed files from a folder that isn't FLEX's")
	}
	dir = filepath.Join(t.TempDir(), "FLEX")
	os.MkdirAll(filepath.Join(dir, "FLEX.exe.WebView2", "x"), 0o755)
	os.WriteFile(filepath.Join(dir, "FLEX.exe"), []byte("x"), 0o644)
	removeDir(dir)
	if exists(dir) {
		t.Error("FLEX's folder is still there")
	}
}
