package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Payloads on a jailbroken PS5 do not survive a restart. After the user runs
// the jailbreak again, elfldr is back on port 9021 but websrv (the homebrew
// launcher, port 8080) is not, and the launcher's home-screen tile only shows
// "not connected". ps5Keeper remembers every console that has used FLEX and,
// when its elfldr is up and websrv is not, sends websrv to elfldr.
//
// websrv is downloaded from its GitHub release on first use (pinned version,
// checked against a SHA-256) and cached in the data dir.

const (
	websrvTag    = "v0.34"
	websrvSHA256 = "54730c867c6e1148536fdcb370e63a7762d989ea87b62488ad4caff64d43f263"
	websrvURL    = "https://github.com/ps5-payload-dev/websrv/releases/download/" + websrvTag + "/websrv-ps5.elf"

	elfldrPort = 9021
	websrvPort = 8080
)

type ps5Keeper struct {
	s *Server

	mu    sync.Mutex
	hosts map[string]*ps5Host
}

type ps5Host struct {
	misses   int // consecutive checks with elfldr up and websrv down
	lastSent time.Time
}

func newPS5Keeper(s *Server) *ps5Keeper {
	k := &ps5Keeper{s: s, hosts: map[string]*ps5Host{}}
	if raw, _ := s.db.GetSetting("ps5_hosts"); raw != "" {
		var hosts []string
		json.Unmarshal([]byte(raw), &hosts)
		for _, h := range hosts {
			k.hosts[h] = &ps5Host{}
		}
	}
	for _, h := range s.cfg.PS5Hosts {
		k.hosts[h] = &ps5Host{}
	}
	return k
}

// note records a console that talked to the API. Only LAN addresses are
// kept: a console reaching FLEX over Tailscale is behind a router we cannot
// (and should not) send payloads through.
func (k *ps5Keeper) note(remoteAddr string) {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return
	}
	addr := ap.Addr().Unmap()
	if !addr.IsPrivate() || isTailnetAddr(remoteAddr) {
		return
	}
	host := addr.String()
	k.mu.Lock()
	_, known := k.hosts[host]
	if !known {
		k.hosts[host] = &ps5Host{}
	}
	list := k.list()
	k.mu.Unlock()
	if known {
		return
	}
	k.s.log.Info("ps5: remembering console", "host", host)
	b, _ := json.Marshal(list)
	k.s.db.SetSetting("ps5_hosts", string(b))
	go k.websrvELF(k.s.bg()) // fetch now while the internet is surely there
}

func (k *ps5Keeper) list() []string {
	out := make([]string, 0, len(k.hosts))
	for h := range k.hosts {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

func (k *ps5Keeper) run(ctx context.Context) {
	for {
		k.mu.Lock()
		hosts := k.list()
		k.mu.Unlock()
		for _, h := range hosts {
			k.check(ctx, h)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Second):
		}
	}
}

func (k *ps5Keeper) check(ctx context.Context, host string) {
	if portOpen(host, websrvPort) || !portOpen(host, elfldrPort) {
		k.update(host, func(h *ps5Host) { h.misses = 0 })
		return
	}
	// elfldr up, websrv down: the console was restarted and jailbroken again.
	send := false
	k.update(host, func(h *ps5Host) {
		h.misses++
		// two checks in a row so a websrv that is merely slow to answer is left
		// alone, and at most one attempt every two minutes
		if h.misses >= 2 && time.Since(h.lastSent) > 2*time.Minute {
			h.lastSent = time.Now()
			h.misses = 0
			send = true
		}
	})
	if !send {
		return
	}
	elf, err := k.websrvELF(ctx)
	if err != nil {
		k.s.log.Warn("ps5: cannot get websrv", "err", err)
		return
	}
	k.s.log.Info("ps5: console restarted, starting the homebrew launcher", "host", host)
	if err := sendPayload(host, elf); err != nil {
		k.s.log.Warn("ps5: sending websrv failed", "host", host, "err", err)
		return
	}
	for i := 0; i < 15; i++ {
		if portOpen(host, websrvPort) {
			k.s.log.Info("ps5: homebrew launcher is up", "host", host)
			return
		}
		time.Sleep(time.Second)
	}
	k.s.log.Warn("ps5: websrv did not come up", "host", host)
}

func (k *ps5Keeper) update(host string, f func(*ps5Host)) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if h, ok := k.hosts[host]; ok {
		f(h)
	}
}

// websrvELF returns the cached websrv payload, downloading it if needed.
func (k *ps5Keeper) websrvELF(ctx context.Context) ([]byte, error) {
	path := filepath.Join(k.s.cfg.DataDir, "ps5", "websrv-"+websrvTag+".elf")
	if b, err := os.ReadFile(path); err == nil && sha(b) == websrvSHA256 {
		return b, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, websrvURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download websrv: HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if got := sha(b); got != websrvSHA256 {
		return nil, fmt.Errorf("websrv checksum mismatch: %s", got)
	}
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		k.s.log.Warn("ps5: cannot cache websrv", "err", err)
	}
	return b, nil
}

// sendPayload hands an ELF to elfldr. elfldr runs it and relays its output
// on the same connection; a few seconds of that are read and dropped, and
// closing the connection leaves the payload running.
func sendPayload(host string, elf []byte) error {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprint(elfldrPort)), 5*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := c.Write(elf); err != nil {
		return err
	}
	if tc, ok := c.(*net.TCPConn); ok {
		tc.CloseWrite()
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	io.Copy(io.Discard, c)
	return nil
}

func portOpen(host string, port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprint(port)), 3*time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func sha(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
