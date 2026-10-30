package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"flex/server"
)

// The desktop app updates itself from the GitHub releases (see
// server/update.go). Windows lets a running .exe be renamed but not
// overwritten, so the new FLEX.exe is written next to the old one, the old
// one is renamed to FLEX.exe.old, and the new one is started with
// --updated-from <pid>: it waits for this process to exit (the single
// instance lock would otherwise hand control back to it) and deletes the
// .old file.

const exeAsset = "FLEX.exe"

type UpdateInfo struct {
	Current   string `json:"current"`
	Latest    string `json:"latest,omitempty"`
	Available bool   `json:"available"`
	URL       string `json:"url,omitempty"` // release page
	Checked   string `json:"checked,omitempty"`
	Error     string `json:"error,omitempty"`
}

type updater struct {
	mu      sync.Mutex
	rel     *server.Release
	info    UpdateInfo
	running bool // an install is in progress
}

// CheckUpdate asks GitHub for the newest release (bound to the frontend).
func (a *App) CheckUpdate() UpdateInfo {
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()
	info := UpdateInfo{Current: server.Version, Checked: time.Now().Format("15:04")}
	rel, err := server.LatestRelease(ctx)
	if err != nil {
		info.Error = err.Error()
	} else {
		info.Latest, info.URL = rel.Version, rel.URL
		info.Available = server.NewerVersion(rel.Version, server.Version) && rel.Has(exeAsset)
	}
	a.up.mu.Lock()
	if err == nil {
		a.up.rel = rel
	}
	a.up.info = info
	a.up.mu.Unlock()
	return info
}

// UpdateStatus is the result of the last check, without asking GitHub.
func (a *App) UpdateStatus() UpdateInfo {
	a.up.mu.Lock()
	defer a.up.mu.Unlock()
	return a.up.info
}

// InstallUpdate downloads the new FLEX.exe, swaps it in and restarts into it.
func (a *App) InstallUpdate() error {
	a.up.mu.Lock()
	rel, busy := a.up.rel, a.up.running
	if rel == nil || busy || !server.NewerVersion(rel.Version, server.Version) {
		a.up.mu.Unlock()
		if busy {
			return errors.New("an update is already being installed")
		}
		return errors.New("no update to install; check for updates first")
	}
	a.up.running = true
	a.up.mu.Unlock()
	defer func() {
		a.up.mu.Lock()
		a.up.running = false
		a.up.mu.Unlock()
	}()

	a.log.Info("update: downloading", "version", rel.Version)
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	b, err := rel.Download(a.ctx, exeAsset)
	if err != nil {
		return err
	}
	next, old := exe+".new", exe+".old"
	if err := os.WriteFile(next, b, 0o755); err != nil {
		return fmt.Errorf("write %s: %w", next, err)
	}
	os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		os.Remove(next)
		return fmt.Errorf("move the running FLEX.exe aside: %w", err)
	}
	if err := os.Rename(next, exe); err != nil {
		os.Rename(old, exe)
		return fmt.Errorf("put the new FLEX.exe in place: %w", err)
	}
	cmd := exec.Command(exe, "--minimized", "--updated-from", strconv.Itoa(os.Getpid()))
	if err := cmd.Start(); err != nil {
		os.Rename(exe, next)
		os.Rename(old, exe)
		return fmt.Errorf("start the new FLEX.exe: %w", err)
	}
	a.log.Info("update: restarting", "version", rel.Version)
	a.Quit()
	return nil
}

// watchUpdates checks at startup and every few hours, for the tray line.
func (a *App) watchUpdates() {
	for {
		info := a.CheckUpdate()
		if info.Error != "" {
			a.log.Warn("update check", "err", info.Error)
		}
		time.Sleep(6 * time.Hour)
	}
}

// afterUpdate runs in a freshly updated FLEX.exe before the window opens:
// wait for the old process to go away, then remove its file.
func afterUpdate(args []string) {
	for i, a := range args {
		if a != "--updated-from" || i+1 >= len(args) {
			continue
		}
		if pid, err := strconv.Atoi(args[i+1]); err == nil {
			waitExit(pid, 30*time.Second)
		}
		if exe, err := os.Executable(); err == nil {
			for try := 0; try < 10 && os.Remove(exe+".old") != nil && fileExists(exe+".old"); try++ {
				time.Sleep(500 * time.Millisecond)
			}
		}
	}
}

func waitExit(pid int, limit time.Duration) {
	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	done := make(chan struct{})
	go func() { p.Wait(); close(done) }() // on Windows this waits for any process
	select {
	case <-done:
	case <-time.After(limit):
	}
	time.Sleep(500 * time.Millisecond) // let the ports and the instance lock go
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
