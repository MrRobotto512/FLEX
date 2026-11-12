package server

// Linked servers. Every FLEX server on the home network finds the others with
// the broadcast clients use (plus any listed in FLEX_PEERS) and lists their
// movies, shows and Continue Watching next to its own, so a client sees the
// whole library whichever server it talks to. Playing, images and progress
// for a linked item are passed on to the server that has the file, which
// converts it on its own GPU.
//
// A broadcast stays on its own network, so a PC behind another router (one
// on the internet provider's Wi-Fi, the other on the home router's) would
// drop out. Each server therefore remembers where the others answered, and
// also asks the devices on its tailnet: first at the direct address
// Tailscale reaches them on (often reachable without Tailscale, and faster),
// then at their Tailscale address.
//
// A linked item's id is its owner's id plus slot*peerStride, where the slot is
// a small number this server keeps for each server it links (saved, so ids
// stay put across restarts; 20 slots keep every id below 2^31 for the PS5).
// A linked conversion's session is "p<slot>-<key>", so its URL keeps the
// /api/v1/hls/<session>/... shape clients take apart.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"tailscale.com/client/tailscale"
	"tailscale.com/ipn/ipnstate"
)

const (
	peerStride   = 100_000_000
	maxPeerSlots = 20
	// peerHeader marks a server-to-server request: answer with this server's
	// own items only, so two linked servers don't list each other's twice.
	peerHeader  = "X-Flex-Peer"
	peerEvery   = time.Minute      // how often to look for servers
	peerGone    = 3 * peerEvery    // not seen for this long: left out
	peerBackoff = 30 * time.Second // after a request to it failed
	// addresses kept per linked server, of each kind (home network, tailnet)
	peerAddrsKept = 2
)

// PeerInfo is a linked server as GET /server and the desktop app show it.
type PeerInfo struct {
	Name    string `json:"name"`
	ID      string `json:"id"`
	Addr    string `json:"addr"`
	Version string `json:"version"`
	Movies  int    `json:"movies"`
	Shows   int    `json:"shows"`
	Online  bool   `json:"online"`
	Via     string `json:"via,omitempty"` // "lan" or "tailscale": how this server reaches it
}

type peer struct {
	id, name, version string
	addr              string // host:port of its API
	slot              int
	seen, failed      time.Time
	movies, shows     int
}

type peers struct {
	s  *Server
	tr *http.Transport

	mu    sync.Mutex
	byID  map[string]*peer
	slots map[string]int      // server id -> slot, kept in the settings table
	known map[string][]string // server id -> where it answered, kept as "peer_addrs"
	round map[string]bool     // linked during the current find (nil outside one)

	status func(context.Context) *ipnstate.Status // the tailnet (tailnetStatus; tests swap it)
}

func newPeers(s *Server) *peers {
	p := &peers{s: s, byID: map[string]*peer{}, slots: map[string]int{}, known: map[string][]string{}}
	p.status = p.tailnetStatus
	p.tr = &http.Transport{
		DialContext:           p.dial,
		ResponseHeaderTimeout: 90 * time.Second, // /play waits for the first segment
		MaxIdleConnsPerHost:   8,
	}
	if raw, _ := s.db.GetSetting("peer_slots"); raw != "" {
		json.Unmarshal([]byte(raw), &p.slots)
	}
	if raw, _ := s.db.GetSetting("peer_addrs"); raw != "" {
		json.Unmarshal([]byte(raw), &p.known)
	}
	return p
}

// dial reaches a linked server; a tailnet address goes through FLEX's own
// Tailscale node when this PC has no Tailscale of its own to route it.
func (p *peers) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, addr)
	if err != nil && isTailnetAddr(addr) {
		if ts := p.s.tsnetServer(); ts != nil {
			return ts.Dial(ctx, network, addr)
		}
	}
	return conn, err
}

// run looks for other servers now and every peerEvery until ctx ends.
func (p *peers) run(ctx context.Context) {
	for {
		p.find(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(peerEvery):
		}
	}
}

// find broadcasts for servers, adds the ones listed in the settings, and asks
// each who it is and how much it holds. Then, for servers the broadcast
// didn't reach: where they answered before, and the tailnet's devices.
func (p *peers) find(ctx context.Context) {
	p.mu.Lock()
	p.round = map[string]bool{}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.round = nil
		p.mu.Unlock()
	}()
	addrs := broadcastDiscover(p.s.cfg.DiscoveryPort, p.s.id, 1500*time.Millisecond)
	for _, a := range p.s.cfg.Peers {
		if _, _, err := net.SplitHostPort(a); err != nil {
			a = net.JoinHostPort(a, "8484")
		}
		addrs = append(addrs, a)
	}
	p.check(ctx, addrs)
	// Direct addresses before Tailscale ones, remembered and the tailnet's
	// devices alike: one PC can be several tailnet devices (the Tailscale
	// app, FLEX's own node), and a Tailscale address mustn't win over a
	// direct one for the same server.
	p.check(ctx, p.remembered(false))
	groups := p.unlinked(tailnetCandidates(p.status(ctx), p.ports()))
	p.checkDevices(ctx, groups, false)
	p.check(ctx, p.remembered(true))
	p.checkDevices(ctx, p.unlinked(groups), true)
}

// checkDevices tries the devices at once, each at its home-network addresses
// (tailnet false) or at its Tailscale ones, in order, until one links.
func (p *peers) checkDevices(ctx context.Context, groups [][]string, tailnet bool) {
	var wg sync.WaitGroup
	for _, g := range groups {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.checkFirst(ctx, g, tailnet)
		}()
	}
	wg.Wait()
}

// check asks the server at each address (host:port) who it is and links it.
func (p *peers) check(ctx context.Context, addrs []string) {
	for _, a := range addrs {
		p.try(ctx, a)
	}
}

// checkFirst tries one device's addresses of one kind in order and stops at
// the first that answers; it and the device's addresses after it are
// remembered for that server.
func (p *peers) checkFirst(ctx context.Context, addrs []string, tailnet bool) {
	for i, a := range addrs {
		if isTailnetAddr(a) != tailnet {
			continue
		}
		if id := p.try(ctx, a); id != "" {
			p.mu.Lock()
			p.rememberLocked(id, addrs[i:]...)
			p.mu.Unlock()
			return
		}
	}
}

// try asks the server at addr who it is and links it; its id, or "" if
// nothing (else) answered there.
func (p *peers) try(ctx context.Context, addr string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var info ServerInfo
	var st ScanStatus
	pe := &peer{addr: addr}
	err := p.getJSON(ctx, pe, "/api/v1/server", &info)
	if err == nil {
		err = p.getJSON(ctx, pe, "/api/v1/library/status", &st)
	}
	if err != nil {
		p.s.log.Debug("linked server not answering", "addr", addr, "err", err)
		return ""
	}
	if info.ID == "" || info.ID == p.s.id {
		return ""
	}
	p.add(info, addr, st)
	return info.ID
}

func (p *peers) add(info ServerInfo, addr string, st ScanStatus) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.round[info.ID] {
		// already linked this round, over an address tried earlier (a better one)
		if pe := p.byID[info.ID]; pe != nil {
			pe.seen = time.Now()
		}
		return
	}
	if p.round != nil {
		p.round[info.ID] = true
	}
	hints := []string{addr}
	if info.Tailscale != nil && info.Tailscale.IP != "" {
		if _, port, err := net.SplitHostPort(addr); err == nil {
			hints = append(hints, net.JoinHostPort(info.Tailscale.IP, port)) // its own tailnet node
		}
	}
	p.rememberLocked(info.ID, hints...)
	pe := p.byID[info.ID]
	if pe == nil {
		slot, ok := p.slots[info.ID]
		if !ok {
			if slot = p.freeSlot(); slot == 0 {
				p.s.log.Warn("too many linked servers", "skipped", info.Name)
				return
			}
			p.slots[info.ID] = slot
			raw, _ := json.Marshal(p.slots)
			p.s.db.SetSetting("peer_slots", string(raw))
		}
		pe = &peer{id: info.ID, slot: slot}
		p.byID[info.ID] = pe
		p.s.log.Info("linked server", "name", info.Name, "addr", addr, "slot", slot)
	}
	if pe.addr != "" && pe.addr != addr {
		p.s.log.Info("linked server moved", "name", info.Name, "from", pe.addr, "to", addr)
	}
	pe.name, pe.version, pe.addr, pe.seen = info.Name, info.Version, addr, time.Now()
	pe.movies, pe.shows = st.Movies, st.Shows
}

// rememberLocked keeps addrs (best first) as where server id answers: the
// newest first, at most peerAddrsKept of each kind, home-network ones ahead
// of tailnet ones. Saved when it changes.
func (p *peers) rememberLocked(id string, addrs ...string) {
	old := p.known[id]
	var lan, tail []string
	for _, a := range append(append([]string{}, addrs...), old...) {
		if slices.Contains(lan, a) || slices.Contains(tail, a) {
			continue
		}
		if isTailnetAddr(a) {
			if len(tail) < peerAddrsKept {
				tail = append(tail, a)
			}
		} else if len(lan) < peerAddrsKept {
			lan = append(lan, a)
		}
	}
	list := append(lan, tail...)
	if slices.Equal(list, old) {
		return
	}
	p.known[id] = list
	raw, _ := json.Marshal(p.known)
	p.s.db.SetSetting("peer_addrs", string(raw))
}

// remembered is where the servers not linked yet this round answered before:
// their Tailscale addresses, or (tailnet false) the others.
func (p *peers) remembered(tailnet bool) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make([]string, 0, len(p.known))
	for id := range p.known {
		if !p.round[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	var out []string
	for _, id := range ids {
		for _, a := range p.known[id] {
			if isTailnetAddr(a) == tailnet {
				out = append(out, a)
			}
		}
	}
	return out
}

// unlinked drops the devices at an address of a server linked this round.
func (p *peers) unlinked(groups [][]string) [][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	owner := map[string]string{}
	for id, addrs := range p.known {
		for _, a := range addrs {
			owner[a] = id
		}
	}
	var out [][]string
	for _, g := range groups {
		if !slices.ContainsFunc(g, func(a string) bool { return p.round[owner[a]] }) {
			out = append(out, g)
		}
	}
	return out
}

// ports are where a linked server may listen: this one's port, and FLEX's own.
func (p *peers) ports() []int {
	out := []int{8484}
	if port := p.s.cfg.HTTPPort; port > 0 && port != 8484 {
		out = append([]int{port}, out...)
	}
	return out
}

// tailnetStatus is the tailnet as the Tailscale app on this PC sees it, else
// as FLEX's own node does; nil without either.
func (p *peers) tailnetStatus(ctx context.Context) *ipnstate.Status {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var app tailscale.LocalClient // the Tailscale app's service, where it is installed
	if st, err := app.Status(ctx); err == nil && st.BackendState == "Running" {
		return st
	}
	if lc := p.s.tsnetClient(); lc != nil {
		if st, err := lc.Status(ctx); err == nil && st.BackendState == "Running" {
			return st
		}
	}
	return nil
}

// tailnetCandidates lists, per online tailnet device that could run FLEX
// (not phones), the addresses to try in order: the private addresses
// Tailscale talks to it on directly (at most two), then its Tailscale
// address, each at every port in ports.
func tailnetCandidates(st *ipnstate.Status, ports []int) [][]string {
	if st == nil {
		return nil
	}
	keys := make([]string, 0, len(st.Peer))
	for k := range st.Peer {
		keys = append(keys, k.String())
	}
	sort.Strings(keys)
	byKey := map[string]*ipnstate.PeerStatus{}
	for k, ps := range st.Peer {
		byKey[k.String()] = ps
	}
	var groups [][]string
	for _, k := range keys {
		ps := byKey[k]
		if ps == nil || !ps.Online || ps.OS == "android" || ps.OS == "iOS" {
			continue
		}
		var g []string
		add := func(ip netip.Addr) bool {
			added := false
			for _, port := range ports {
				if a := netip.AddrPortFrom(ip, uint16(port)).String(); !slices.Contains(g, a) {
					g, added = append(g, a), true
				}
			}
			return added
		}
		direct := 0
		for _, a := range append([]string{ps.CurAddr}, ps.Addrs...) {
			ap, err := netip.ParseAddrPort(a)
			if err != nil || !ap.Addr().Is4() || !ap.Addr().IsPrivate() || direct == 2 {
				continue
			}
			if add(ap.Addr()) {
				direct++
			}
		}
		for _, ip := range ps.TailscaleIPs {
			if ip.Is4() {
				add(ip)
			}
		}
		if len(g) > 0 {
			groups = append(groups, g)
		}
	}
	return groups
}

func (p *peers) freeSlot() int {
	used := map[int]bool{}
	for _, s := range p.slots {
		used[s] = true
	}
	for s := 1; s <= maxPeerSlots; s++ {
		if !used[s] {
			return s
		}
	}
	return 0
}

// online is the linked servers to ask now: seen lately and not failing.
// Copies, so callers need no lock.
func (p *peers) online() []*peer {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*peer
	for _, pe := range p.byID {
		if now.Sub(pe.seen) < peerGone && now.Sub(pe.failed) > peerBackoff {
			c := *pe
			out = append(out, &c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].slot < out[j].slot })
	return out
}

// bySlot is the server behind a linked id, if it has been seen lately.
func (p *peers) bySlot(slot int) *peer {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pe := range p.byID {
		if pe.slot == slot && time.Since(pe.seen) < peerGone {
			c := *pe
			return &c
		}
	}
	return nil
}

func (p *peers) fail(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pe := p.byID[id]; pe != nil {
		pe.failed = time.Now()
	}
}

func (p *peers) info() []PeerInfo {
	online := map[string]bool{}
	for _, pe := range p.online() {
		online[pe.id] = true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []PeerInfo{}
	for _, pe := range p.byID {
		via := "lan"
		if isTailnetAddr(pe.addr) {
			via = "tailscale"
		}
		out = append(out, PeerInfo{Name: pe.name, ID: pe.id, Addr: pe.addr, Version: pe.version,
			Movies: pe.movies, Shows: pe.shows, Online: online[pe.id], Via: via})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ---------------------------------------------------------------- talking to them

// request builds a call to pe; from, if set, is the client request whose body
// it carries on.
func (p *peers) request(ctx context.Context, method string, pe *peer, path string, from *http.Request) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://"+pe.addr+path, nil)
	if err != nil {
		return nil, err
	}
	if from != nil && from.ContentLength != 0 {
		req.Body, req.ContentLength = from.Body, from.ContentLength
		if ct := from.Header.Get("Content-Type"); ct != "" {
			req.Header.Set("Content-Type", ct)
		}
	}
	req.Header.Set(peerHeader, p.s.id)
	req.Header.Set("X-Flex-Client", "peer") // not "ps5": the other server would take us for a console
	return req, nil
}

func (p *peers) getJSON(ctx context.Context, pe *peer, path string, v any) error {
	req, err := p.request(ctx, http.MethodGet, pe, path, nil)
	if err != nil {
		return err
	}
	resp, err := p.tr.RoundTrip(req)
	if err != nil {
		p.fail(pe.id)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// gather asks every linked server for the list at path at once and hands
// each answer to add, one at a time. A server that doesn't answer within a
// few seconds is left out (and rested for peerBackoff).
func gather[T any](ctx context.Context, p *peers, path string, add func(pe *peer, items []T)) {
	ps := p.online()
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	type answer struct {
		pe    *peer
		items []T
	}
	ch := make(chan answer, len(ps))
	for _, pe := range ps {
		go func() {
			var items []T
			if err := p.getJSON(ctx, pe, path, &items); err != nil {
				p.s.log.Warn("linked server", "name", pe.name, "path", path, "err", err)
				items = nil
			}
			ch <- answer{pe, items}
		}()
	}
	for range ps {
		if a := <-ch; a.items != nil {
			add(a.pe, a.items)
		}
	}
}

// ---------------------------------------------------------------- ids and URLs

// global is a linked server's id as this server hands it out; 0 if it
// doesn't fit (never for a real library).
func (pe *peer) global(id int64) int64 {
	if id <= 0 || id >= peerStride {
		return 0
	}
	return int64(pe.slot)*peerStride + id
}

// splitID tells a linked id from a local one: slot 0 is this server.
func splitID(id int64) (slot int, local int64) { return int(id / peerStride), id % peerStride }

var (
	itemPathRe   = regexp.MustCompile(`^/api/v1/(movies|shows|episodes)/(\d+)([/?].*)?$`)
	hlsPathRe    = regexp.MustCompile(`^/api/v1/hls/([^/?]+)(.*)$`)
	linkedHLSRe  = regexp.MustCompile(`^/api/v1/hls/p(\d+)-([^/]+)(/.*)?$`)
	peerSafeRest = map[string]bool{"": true, "/poster": true, "/backdrop": true, "/still": true,
		"/play": true, "/file": true, "/progress": true, "/match": true, "/files": true}
)

// url turns a path a linked server gave out into one this server answers.
func (pe *peer) url(u string) string {
	if m := itemPathRe.FindStringSubmatch(u); m != nil {
		id, _ := strconv.ParseInt(m[2], 10, 64)
		return fmt.Sprintf("/api/v1/%s/%d%s", m[1], pe.global(id), m[3])
	}
	if m := hlsPathRe.FindStringSubmatch(u); m != nil {
		return fmt.Sprintf("/api/v1/hls/p%d-%s%s", pe.slot, m[1], m[2])
	}
	return u
}

func (pe *peer) movie(m *Movie) {
	m.ID = pe.global(m.ID)
	m.Poster, m.Backdrop = pe.url(m.Poster), pe.url(m.Backdrop)
}

func (pe *peer) show(sh *Show) {
	sh.ID = pe.global(sh.ID)
	sh.Poster, sh.Backdrop = pe.url(sh.Poster), pe.url(sh.Backdrop)
	for i := range sh.SeasonList {
		for j := range sh.SeasonList[i].Episodes {
			pe.episode(&sh.SeasonList[i].Episodes[j])
		}
	}
	if sh.NextEpisode != nil {
		pe.episode(sh.NextEpisode)
	}
}

func (pe *peer) episode(e *Episode) {
	e.ID, e.ShowID = pe.global(e.ID), pe.global(e.ShowID)
	e.Still = pe.url(e.Still)
}

func (pe *peer) continueItem(c *ContinueItem) {
	c.ID, c.PosterID = pe.global(c.ID), pe.global(c.PosterID)
	if c.ShowID != 0 {
		c.ShowID = pe.global(c.ShowID)
	}
	c.Poster = pe.url(c.Poster)
}

// ---------------------------------------------------------------- merged lists

// linking says whether a request gets the linked servers' items as well: not
// when it comes from one of them.
func (s *Server) linking(r *http.Request) bool {
	return s.peers != nil && r.Header.Get(peerHeader) == ""
}

func (s *Server) linkedMovies(r *http.Request, movies []Movie) []Movie {
	movies = s.peerMovies(r.Context(), movies, r.URL.RawQuery)
	sortMovies(movies, r.URL.Query().Get("sort"))
	return movies
}

// peerMovies adds the linked servers' movies (their /movies?query) to movies.
func (s *Server) peerMovies(ctx context.Context, movies []Movie, query string) []Movie {
	gather(ctx, s.peers, "/api/v1/movies?"+query, func(pe *peer, list []Movie) {
		for _, m := range list {
			if pe.movie(&m); m.ID != 0 {
				movies = append(movies, m)
			}
		}
	})
	return movies
}

// sortMovies puts a merged list in the order ListMovies uses.
func sortMovies(movies []Movie, by string) {
	title := func(i, j int) bool { return strings.ToLower(movies[i].Title) < strings.ToLower(movies[j].Title) }
	sort.SliceStable(movies, func(i, j int) bool {
		switch by {
		case "added":
			return movies[i].AddedAt.After(movies[j].AddedAt)
		case "year":
			if movies[i].Year != movies[j].Year {
				return movies[i].Year > movies[j].Year
			}
		}
		return title(i, j)
	})
}

func (s *Server) linkedShows(r *http.Request, shows []Show) []Show {
	shows = s.peerShows(r.Context(), shows, r.URL.RawQuery)
	sort.SliceStable(shows, func(i, j int) bool { return strings.ToLower(shows[i].Title) < strings.ToLower(shows[j].Title) })
	return shows
}

func (s *Server) peerShows(ctx context.Context, shows []Show, query string) []Show {
	gather(ctx, s.peers, "/api/v1/shows?"+query, func(pe *peer, list []Show) {
		for _, sh := range list {
			if pe.show(&sh); sh.ID != 0 {
				shows = append(shows, sh)
			}
		}
	})
	return shows
}

// peerSeasonal adds the linked servers' holiday episodes.
func (s *Server) peerSeasonal(ctx context.Context, eps []Episode) []Episode {
	gather(ctx, s.peers, "/api/v1/library/seasonal", func(pe *peer, list []Episode) {
		for _, e := range list {
			if pe.episode(&e); e.ID != 0 {
				e.Poster, e.Backdrop = pe.url(e.Poster), pe.url(e.Backdrop)
				eps = append(eps, e)
			}
		}
	})
	return eps
}

func (s *Server) linkedContinue(r *http.Request, items []ContinueItem, limit int) []ContinueItem {
	gather(r.Context(), s.peers, fmt.Sprintf("/api/v1/continue?limit=%d", limit), func(pe *peer, list []ContinueItem) {
		for _, c := range list {
			if pe.continueItem(&c); c.ID != 0 {
				items = append(items, c)
			}
		}
	})
	sort.SliceStable(items, func(i, j int) bool { return items[i].Played.After(items[j].Played) })
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}

// ---------------------------------------------------------------- passing requests on

// linkedItems passes requests for a linked server's items (ids from slot 1
// up) and conversions ("p<slot>-..." sessions) on to that server.
func (s *Server) linkedItems(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.linking(r) {
			next.ServeHTTP(w, r)
			return
		}
		if m := itemPathRe.FindStringSubmatch(r.URL.Path); m != nil && peerSafeRest[m[3]] {
			id, _ := strconv.ParseInt(m[2], 10, 64)
			if slot, local := splitID(id); slot > 0 {
				s.forwardItem(w, r, slot, m[1], local, m[3])
				return
			}
		}
		if m := linkedHLSRe.FindStringSubmatch(r.URL.Path); m != nil {
			slot, _ := strconv.Atoi(m[1])
			if pe := s.peers.bySlot(slot); pe != nil {
				s.peers.proxy(w, r, pe, "/api/v1/hls/"+m[2]+m[3])
			} else {
				writeErr(w, http.StatusNotFound, "not_found", "that server is not linked right now")
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) forwardItem(w http.ResponseWriter, r *http.Request, slot int, kind string, id int64, rest string) {
	pe := s.peers.bySlot(slot)
	if pe == nil {
		writeErr(w, http.StatusNotFound, "not_found", "that server is not linked right now")
		return
	}
	path := fmt.Sprintf("/api/v1/%s/%d%s", kind, id, rest)
	switch rest {
	case "", "/match":
		switch kind {
		case "movies":
			var m Movie
			s.peers.relayJSON(w, r, pe, path, &m, func() { pe.movie(&m) })
		case "shows":
			var sh Show
			s.peers.relayJSON(w, r, pe, path, &sh, func() { pe.show(&sh) })
		default:
			var e Episode
			s.peers.relayJSON(w, r, pe, path, &e, func() { pe.episode(&e) })
		}
	case "/play":
		// decide the profile here: the other server sees us, not the client
		q := r.URL.Query()
		q.Set("profile", playProfile(r))
		var info PlayInfo
		s.peers.relayJSON(w, r, pe, path+"?"+q.Encode(), &info, func() { info.URL = pe.url(info.URL) })
	case "/files":
		if isTailnetAddr(r.RemoteAddr) {
			writeErr(w, http.StatusForbidden, "forbidden", "only available on the home network")
			return
		}
		s.peers.proxy(w, r, pe, path)
	default: // images, the file itself, progress
		s.peers.proxy(w, r, pe, path)
	}
}

// relayJSON sends the client's request to pe and answers with its JSON
// after fix has rewritten the ids in v.
func (p *peers) relayJSON(w http.ResponseWriter, r *http.Request, pe *peer, path string, v any, fix func()) {
	req, err := p.request(r.Context(), r.Method, pe, path, r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	resp, err := p.tr.RoundTrip(req)
	if err != nil {
		p.fail(pe.id)
		writeErr(w, http.StatusBadGateway, "peer_unreachable", pe.name+": "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e map[string]string
		json.NewDecoder(resp.Body).Decode(&e)
		writeErr(w, resp.StatusCode, e["error"], pe.name+": "+e["message"])
		return
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadGateway, "peer_unreachable", pe.name+": "+err.Error())
		return
	}
	fix()
	writeJSON(w, http.StatusOK, v)
}

// proxy streams the request to pe at path and its answer back: images, whole
// files with Range, HLS playlists and segments, progress.
func (p *peers) proxy(w http.ResponseWriter, r *http.Request, pe *peer, path string) {
	rp := &httputil.ReverseProxy{
		Transport: p.tr,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.Host = "http", pe.addr, pe.addr
			pr.Out.URL.Path, pr.Out.URL.RawPath = path, ""
			pr.Out.Header.Set(peerHeader, p.s.id)
			pr.Out.Header.Set("X-Flex-Client", "peer")
		},
		ModifyResponse: func(resp *http.Response) error {
			for k := range resp.Header { // ours are set already
				if strings.HasPrefix(k, "Access-Control-") {
					resp.Header.Del(k)
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return // the client went away
			}
			p.fail(pe.id)
			writeErr(w, http.StatusBadGateway, "peer_unreachable", pe.name+": "+err.Error())
		},
	}
	rp.ServeHTTP(w, r)
}

// ---------------------------------------------------------------- discovery

// broadcastDiscover sends the clients' discovery datagram to every IPv4
// broadcast address this machine has (a plain 255.255.255.255 can leave
// through the wrong adapter) and returns the API address of each server that
// answers within wait, except the one with id self.
func broadcastDiscover(port int, self string, wait time.Duration) []string {
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return nil
	}
	defer conn.Close()
	for _, ip := range broadcastAddrs() {
		conn.WriteToUDP([]byte(discoverMagic+" 1"), &net.UDPAddr{IP: ip, Port: port})
	}
	conn.SetReadDeadline(time.Now().Add(wait))
	seen := map[string]bool{}
	var out []string
	buf := make([]byte, 512)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			return out
		}
		var reply struct {
			ID   string `json:"id"`
			Port int    `json:"port"`
		}
		if json.Unmarshal(buf[:n], &reply) != nil || reply.Port == 0 || reply.ID == self {
			continue
		}
		if a := net.JoinHostPort(from.IP.String(), strconv.Itoa(reply.Port)); !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
}

func broadcastAddrs() []net.IP {
	out := []net.IP{net.IPv4bcast}
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagBroadcast == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok || n.IP.To4() == nil {
				continue
			}
			ip := n.IP.To4()
			mask := n.Mask
			if len(mask) == net.IPv6len {
				mask = mask[12:]
			}
			b := make(net.IP, net.IPv4len)
			for i := range b {
				b[i] = ip[i] | ^mask[i]
			}
			out = append(out, b)
		}
	}
	return out
}
