package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/types/key"
)

func peersForTest(t *testing.T) (*Server, *peers) {
	t.Helper()
	d, err := OpenDB(filepath.Join(t.TempDir(), "flex.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	s := &Server{cfg: Config{HTTPPort: 8484}, db: d, id: "front", log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.peers = newPeers(s)
	s.peers.status = func(context.Context) *ipnstate.Status { return nil } // never the real tailnet
	return s, s.peers
}

func TestTailnetCandidates(t *testing.T) {
	ps := func(online bool, os, cur string, addrs []string, ips ...string) *ipnstate.PeerStatus {
		var tips []netip.Addr
		for _, ip := range ips {
			tips = append(tips, netip.MustParseAddr(ip))
		}
		return &ipnstate.PeerStatus{Online: online, OS: os, CurAddr: cur, Addrs: addrs, TailscaleIPs: tips}
	}
	garage := ps(true, "windows", "192.168.1.245:41641",
		[]string{"192.168.1.245:41641", "172.20.0.5:41641", "10.0.0.9:41641", "203.0.113.7:41641"},
		"100.118.138.25", "fd7a:115c:a1e0::1")
	relayed := ps(true, "windows", "", nil, "100.68.46.78")
	phone := ps(true, "android", "192.168.0.50:41641", nil, "100.88.77.79")
	off := ps(false, "macOS", "", nil, "100.90.233.79")
	st := &ipnstate.Status{Peer: map[key.NodePublic]*ipnstate.PeerStatus{}}
	for _, p := range []*ipnstate.PeerStatus{garage, relayed, phone, off} {
		st.Peer[key.NewNode().Public()] = p
	}

	got := tailnetCandidates(st, []int{8484})
	want := map[string]bool{
		// the direct addresses first (two at most, private only), then the tailnet one
		"192.168.1.245:8484 172.20.0.5:8484 100.118.138.25:8484": true,
		"100.68.46.78:8484": true,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d devices %v, want the garage PC and the relayed one (no phone, nothing offline)", len(got), got)
	}
	for _, g := range got {
		if !want[strings.Join(g, " ")] {
			t.Errorf("unexpected order or addresses: %v", g)
		}
	}

	two := tailnetCandidates(&ipnstate.Status{Peer: map[key.NodePublic]*ipnstate.PeerStatus{key.NewNode().Public(): relayed}},
		[]int{8485, 8484})
	if len(two) != 1 || !slices.Equal(two[0], []string{"100.68.46.78:8485", "100.68.46.78:8484"}) {
		t.Errorf("each address at every port, this server's first: %v", two)
	}
	if tailnetCandidates(nil, []int{8484}) != nil {
		t.Error("no tailnet, no candidates")
	}
}

func TestPeerAddressesRemembered(t *testing.T) {
	s, p := peersForTest(t)
	p.mu.Lock()
	p.rememberLocked("garage1", "100.118.138.25:8484", "192.168.1.245:8484")
	p.rememberLocked("garage1", "192.168.0.107:8484")
	p.rememberLocked("garage1", "192.168.1.9:8484") // the oldest home-network one goes
	p.mu.Unlock()
	want := []string{"192.168.1.9:8484", "192.168.0.107:8484", "100.118.138.25:8484"}
	if got := p.known["garage1"]; !slices.Equal(got, want) {
		t.Fatalf("known = %v, want %v (newest first, home network ahead of tailnet, two of each)", got, want)
	}
	if again := newPeers(s); !slices.Equal(again.known["garage1"], want) {
		t.Errorf("not kept across a restart: %v", again.known["garage1"])
	}

	p.round = map[string]bool{"garage1": true}
	if got := append(p.remembered(false), p.remembered(true)...); len(got) != 0 {
		t.Errorf("a server linked this round isn't asked again: %v", got)
	}
	groups := [][]string{{"10.0.0.2:8484", "100.118.138.25:8484"}, {"100.1.2.3:8484"}}
	if got := p.unlinked(groups); len(got) != 1 || got[0][0] != "100.1.2.3:8484" {
		t.Errorf("the device at a linked server's address is skipped, others kept: %v", got)
	}
	p.round = map[string]bool{}
	if got := append(p.remembered(false), p.remembered(true)...); !slices.Equal(got, want) {
		t.Errorf("remembered = %v, want %v", got, want)
	}
}

func TestPeerLinksOverFirstAnsweringAddress(t *testing.T) {
	_, p := peersForTest(t)
	a := httptest.NewServer(&fakePeer{})
	defer a.Close()
	b := httptest.NewServer(&fakePeer{}) // the same server ("garage1") on a second address
	defer b.Close()
	addrA, addrB := strings.TrimPrefix(a.URL, "http://"), strings.TrimPrefix(b.URL, "http://")

	// nothing listens here: refused straight away
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()

	p.round = map[string]bool{}
	p.checkFirst(context.Background(), []string{dead, addrA, "100.64.0.9:8484"}, false)
	p.check(context.Background(), []string{addrB}) // a worse address, later in the same round
	info := p.info()
	if len(info) != 1 || info[0].Addr != addrA || !info[0].Online || info[0].Via != "lan" {
		t.Fatalf("want the garage linked at %s over the home network: %+v", addrA, info)
	}
	if got := p.known["garage1"]; !slices.Equal(got, []string{addrA, "100.64.0.9:8484"}) {
		t.Errorf("where it answered and the addresses after it are kept, the dead one not: %v", got)
	}
	slot := p.byID["garage1"].slot
	// the Tailscale pass that follows skips that device, but not one it hasn't placed
	if left := p.unlinked([][]string{{dead, addrA, "100.64.0.9:8484"}, {"100.64.0.10:8484"}}); len(left) != 1 ||
		left[0][0] != "100.64.0.10:8484" {
		t.Errorf("after the direct pass: %v", left)
	}

	// next round it answers only at the second address: it moves, same slot
	p.round = map[string]bool{}
	p.check(context.Background(), []string{addrB})
	if pe := p.byID["garage1"]; pe.addr != addrB || pe.slot != slot {
		t.Errorf("moved server: addr %s slot %d, want %s slot %d", pe.addr, pe.slot, addrB, slot)
	}
}

func TestPlayProfileForLinkedServers(t *testing.T) {
	req := func(from string, peerHdr bool) string {
		r := httptest.NewRequest("GET", "/api/v1/movies/1/play?profile=ps5", nil)
		r.RemoteAddr = from
		if peerHdr {
			r.Header.Set(peerHeader, "robot")
		}
		return playProfile(r)
	}
	if got := req("100.88.91.13:50000", false); got != "remote" {
		t.Errorf("a device away from home gets the remote profile, got %q", got)
	}
	if got := req("100.88.91.13:50000", true); got != "ps5" {
		t.Errorf("a linked server over the tailnet keeps the profile it chose for its console, got %q", got)
	}
	if got := req("192.168.1.106:50000", true); got != "ps5" {
		t.Errorf("home network: %q", got)
	}
}
