package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/vishen/go-chromecast/application"
	"golang.org/x/net/ipv4"
)

// Chromecast: the server finds the Cast devices on the network itself and
// hands one a stream, so every client (browser, PS5, the Android app, the
// Windows player) gets a "Cast to…" choice without needing Chrome. The
// device fetches the stream from this server like any client would (the
// "cast" profile: H.264 up to 1080p, AAC; an MP4 as it is, HLS otherwise),
// and the server keeps the resume point up to date while it plays.

// CastDevice is a Chromecast (or a TV with Cast built in) on the network.
type CastDevice struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Model string `json:"model,omitempty"`
	Addr  string `json:"addr"`
	Port  int    `json:"-"`
}

// CastStatus is what is playing on a Chromecast through this server.
type CastStatus struct {
	Casting   bool    `json:"casting"`
	Device    string  `json:"device,omitempty"`
	DeviceID  string  `json:"device_id,omitempty"`
	Kind      string  `json:"kind,omitempty"`
	ID        int64   `json:"id,omitempty"`
	Title     string  `json:"title,omitempty"`
	State     string  `json:"state,omitempty"` // playing | paused | buffering | idle
	PositionS float64 `json:"position_s,omitempty"`
	DurationS float64 `json:"duration_s,omitempty"`
	Method    string  `json:"method,omitempty"` // direct | hls
}

type castSession struct {
	dev    CastDevice
	kind   string
	id     int64
	title  string
	info   PlayInfo
	offset float64 // an HLS session starts at the asked position; the device counts from 0
	hlsKey string
	state  string
	pos    float64
	app    *application.Application
	done   chan struct{}
}

type caster struct {
	s       *Server
	mu      sync.Mutex
	devices map[string]CastDevice
	scanned time.Time
	cur     *castSession
}

const castScanFor = 3 * time.Second

func newCaster(s *Server) *caster {
	return &caster{s: s, devices: map[string]CastDevice{}}
}

// Devices lists the Cast devices seen on the network: a fresh 3 s mDNS
// scan when asked, when nothing is known yet, or when the list is old.
func (c *caster) Devices(ctx context.Context, fresh bool) []CastDevice {
	c.mu.Lock()
	stale := fresh || len(c.devices) == 0 || time.Since(c.scanned) > 2*time.Minute
	c.mu.Unlock()
	if stale {
		c.scan(ctx)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]CastDevice, 0, len(c.devices))
	for _, d := range c.devices {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// scan asks for _googlecast._tcp with a one-shot mDNS query out of every
// network interface: a PC with VPN and virtual adapters routes multicast
// to the wrong one by default (a Hyper-V switch here), and the question
// goes from an ordinary port, so each device answers it by unicast, which
// the firewall lets back in without a rule.
func (c *caster) scan(ctx context.Context) {
	ifs, _ := net.Interfaces()
	answers := make(chan CastDevice, 64)
	var wg sync.WaitGroup
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagMulticast == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
				wg.Add(1)
				go func(i net.Interface, ip net.IP) {
					defer wg.Done()
					castQuery(ctx, i, ip, answers)
				}(i, n.IP.To4())
			}
		}
	}
	go func() { wg.Wait(); close(answers) }()
	found := map[string]CastDevice{}
	for d := range answers {
		if d.Model == "Google Cast Group" { // speaker groups: no screen
			continue
		}
		found[d.ID] = d
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(found) > 0 || time.Since(c.scanned) > 10*time.Minute {
		c.devices = found // a missed answer keeps the old list a while; a long silence clears it
	}
	c.scanned = time.Now()
}

func castQuery(ctx context.Context, iface net.Interface, ip net.IP, out chan<- CastDevice) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip, Port: 0})
	if err != nil {
		return
	}
	defer conn.Close()
	_ = ipv4.NewPacketConn(conn).SetMulticastInterface(&iface)
	m := new(dns.Msg)
	m.SetQuestion("_googlecast._tcp.local.", dns.TypePTR)
	m.RecursionDesired = false
	m.Question[0].Qclass |= 1 << 15 // "answer me by unicast"
	q, _ := m.Pack()
	dst := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
	for i := 0; i < 2; i++ {
		_, _ = conn.WriteToUDP(q, dst)
		time.Sleep(250 * time.Millisecond)
	}
	deadline := time.Now().Add(castScanFor)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetReadDeadline(deadline)
	buf := make([]byte, 9000)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		var r dns.Msg
		if r.Unpack(buf[:n]) != nil {
			continue
		}
		d := CastDevice{Addr: from.IP.String(), Port: 8009}
		for _, rr := range append(r.Answer, r.Extra...) {
			switch v := rr.(type) {
			case *dns.TXT:
				for _, t := range v.Txt {
					k, val, _ := strings.Cut(t, "=")
					switch k {
					case "fn":
						d.Name = val
					case "md":
						d.Model = val
					case "id":
						d.ID = val
					}
				}
			case *dns.SRV:
				d.Port = int(v.Port)
			case *dns.A:
				d.Addr = v.A.String()
			}
		}
		if d.ID == "" {
			d.ID = d.Addr
		}
		if d.Name == "" {
			d.Name = d.Addr
		}
		out <- d
	}
}

// Cast starts kind/id on a device from `start` seconds, ending whatever
// this server was casting before.
func (c *caster) Cast(ctx context.Context, deviceID, kind string, id int64, start float64) (CastStatus, error) {
	var dev CastDevice
	var ok bool
	for i := 0; i < 2 && !ok; i++ {
		c.mu.Lock()
		dev, ok = c.devices[deviceID]
		c.mu.Unlock()
		if !ok {
			c.scan(ctx)
		}
	}
	if !ok {
		return CastStatus{}, fmt.Errorf("no Chromecast %q on the network", deviceID)
	}
	c.Stop()

	// the play decision and the title come from this server's own API, so
	// linked titles (another PC's) work the same way they do for any client
	var info PlayInfo
	path := fmt.Sprintf("/api/v1/%ss/%d/play?profile=cast&start=%d", kind, id, int(start))
	if err := c.ownAPI(ctx, "GET", path, nil, &info); err != nil {
		return CastStatus{}, err
	}
	var detail map[string]any
	_ = c.ownAPI(ctx, "GET", fmt.Sprintf("/api/v1/%ss/%d", kind, id), nil, &detail)
	title := castTitle(kind, detail)

	local := localAddrFor(dev.Addr)
	if local == "" {
		return CastStatus{}, fmt.Errorf("no route to %s", dev.Addr)
	}
	url := fmt.Sprintf("http://%s:%d%s", local, c.s.cfg.HTTPPort, info.URL)
	contentType, startAt, offset := "video/mp4", int(start), 0.0
	hlsKey := ""
	if info.Method == "hls" {
		contentType = "application/x-mpegURL"
		startAt, offset = 0, start
		hlsKey = strings.TrimSuffix(strings.TrimPrefix(info.URL, "/api/v1/hls/"), "/index.m3u8")
	}

	app := application.NewApplication(application.WithCacheDisabled(true))
	if err := app.Start(dev.Addr, dev.Port); err != nil {
		c.stopHLS(hlsKey)
		return CastStatus{}, fmt.Errorf("connect to %s: %w", dev.Name, err)
	}
	if err := app.Load(url, startAt, contentType, false, true, true); err != nil {
		app.Close(false)
		c.stopHLS(hlsKey)
		return CastStatus{}, fmt.Errorf("%s: %w", dev.Name, err)
	}
	c.s.log.Info("cast: playing", "device", dev.Name, "title", title, "method", info.Method, "url", url)

	ses := &castSession{dev: dev, kind: kind, id: id, title: title, info: info, offset: offset, hlsKey: hlsKey,
		state: "buffering", pos: start, app: app, done: make(chan struct{})}
	c.mu.Lock()
	c.cur = ses
	c.mu.Unlock()
	go c.watch(ses)
	return c.Status(), nil
}

// watch follows the device's playback: resume point every 10 s, and the
// session torn down when the device stops or finishes.
func (c *caster) watch(ses *castSession) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	lastSave := time.Now()
	for {
		select {
		case <-ses.done:
			return
		case <-tick.C:
		}
		if err := ses.app.Update(); err != nil {
			c.s.log.Warn("cast: lost the device", "device", ses.dev.Name, "err", err)
			c.end(ses, false)
			return
		}
		_, media, _ := ses.app.Status()
		c.mu.Lock()
		if media != nil {
			ses.pos = ses.offset + float64(media.CurrentTime)
			switch media.PlayerState {
			case "PLAYING":
				ses.state = "playing"
			case "PAUSED":
				ses.state = "paused"
			case "BUFFERING":
				ses.state = "buffering"
			case "IDLE":
				ses.state = "idle"
			}
		}
		state, pos := ses.state, ses.pos
		idleWhy := ""
		if media != nil {
			idleWhy = media.IdleReason
		}
		c.mu.Unlock()
		if state == "idle" && idleWhy != "" {
			finished := idleWhy == "FINISHED"
			c.s.log.Info("cast: ended", "device", ses.dev.Name, "title", ses.title, "why", idleWhy)
			c.end(ses, finished)
			return
		}
		if state == "playing" && time.Since(lastSave) >= 10*time.Second {
			c.saveProgress(ses, pos)
			lastSave = time.Now()
		}
	}
}

func (c *caster) saveProgress(ses *castSession, pos float64) {
	body := map[string]int{"position_s": int(pos), "duration_s": int(ses.info.DurationS)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.ownAPI(ctx, "PUT", fmt.Sprintf("/api/v1/%ss/%d/progress", ses.kind, ses.id), body, nil); err != nil {
		c.s.log.Warn("cast: progress", "err", err)
	}
}

// end closes a session: the resume point (the whole length when it played
// to the end), the HLS session, the connection.
func (c *caster) end(ses *castSession, finished bool) {
	c.mu.Lock()
	if c.cur != ses {
		c.mu.Unlock()
		return
	}
	c.cur = nil
	pos := ses.pos
	c.mu.Unlock()
	close(ses.done)
	if finished {
		pos = ses.info.DurationS
	}
	if pos > 0 {
		c.saveProgress(ses, pos)
	}
	ses.app.Close(false)
	c.stopHLS(ses.hlsKey)
}

func (c *caster) stopHLS(key string) {
	if key != "" {
		c.s.tc.Stop(key)
	}
}

// Stop ends the current cast, stopping playback on the device.
func (c *caster) Stop() {
	c.mu.Lock()
	ses := c.cur
	c.mu.Unlock()
	if ses == nil {
		return
	}
	_ = ses.app.StopMedia()
	c.end(ses, false)
}

// Control: pause, play, seek (to position_s). A seek in an HLS session
// means a new session from there, as it does for the PS5.
func (c *caster) Control(ctx context.Context, action string, position float64) (CastStatus, error) {
	c.mu.Lock()
	ses := c.cur
	c.mu.Unlock()
	if ses == nil {
		return CastStatus{}, fmt.Errorf("nothing is being cast")
	}
	var err error
	switch action {
	case "pause":
		if ses.state != "paused" {
			err = ses.app.Pause()
			c.setState(ses, "paused")
		}
	case "play":
		if ses.state == "paused" {
			err = ses.app.TogglePause()
			c.setState(ses, "playing")
		}
	case "seek":
		if position < 0 {
			position = 0
		}
		if ses.info.Method == "hls" {
			return c.Cast(ctx, ses.dev.ID, ses.kind, ses.id, position)
		}
		err = ses.app.SeekToTime(float32(position))
		c.mu.Lock()
		ses.pos = position
		c.mu.Unlock()
	default:
		return CastStatus{}, fmt.Errorf("unknown action %q", action)
	}
	if err != nil {
		return CastStatus{}, fmt.Errorf("%s: %w", action, err)
	}
	return c.Status(), nil
}

func (c *caster) setState(ses *castSession, state string) {
	c.mu.Lock()
	ses.state = state
	c.mu.Unlock()
}

func (c *caster) Status() CastStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	ses := c.cur
	if ses == nil {
		return CastStatus{}
	}
	return CastStatus{Casting: true, Device: ses.dev.Name, DeviceID: ses.dev.ID, Kind: ses.kind, ID: ses.id,
		Title: ses.title, State: ses.state, PositionS: ses.pos, DurationS: ses.info.DurationS, Method: ses.info.Method}
}

func (c *caster) Close() { c.Stop() }

// ownAPI calls this server's own HTTP API: the same answers a client gets.
func (c *caster) ownAPI(ctx context.Context, method, path string, body any, out any) error {
	var rd *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	req, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", c.s.cfg.HTTPPort, path), rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-Flex-Client", "cast")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Message == "" {
			e.Message = resp.Status
		}
		return fmt.Errorf("%s %s: %s", method, path, e.Message)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func castTitle(kind string, d map[string]any) string {
	str := func(k string) string { s, _ := d[k].(string); return s }
	num := func(k string) int { f, _ := d[k].(float64); return int(f) }
	if kind == "episode" {
		return fmt.Sprintf("%s · S%02dE%02d · %s", str("show_title"), num("season"), num("episode"), str("title"))
	}
	if y := num("year"); y > 0 {
		return fmt.Sprintf("%s (%d)", str("title"), y)
	}
	return str("title")
}

// localAddrFor is this PC's address on the network that reaches ip: the one
// the device has to fetch the stream from.
func localAddrFor(ip string) string {
	conn, err := net.Dial("udp4", net.JoinHostPort(ip, "8009"))
	if err != nil {
		return ""
	}
	defer conn.Close()
	if a, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return a.IP.String()
	}
	return ""
}

// --- HTTP ---

func (s *Server) handleCastDevices(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.cast.Devices(r.Context(), r.URL.Query().Get("scan") == "1"))
}

func (s *Server) handleCastStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.cast.Status())
}

func (s *Server) handleCast(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Device string  `json:"device"`
		Kind   string  `json:"kind"`
		ID     int64   `json:"id"`
		Start  float64 `json:"start"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Device == "" || body.ID <= 0 ||
		(body.Kind != "movie" && body.Kind != "episode") {
		writeErr(w, http.StatusBadRequest, "bad_request", "device, kind (movie|episode) and id are needed")
		return
	}
	st, err := s.cast.Cast(r.Context(), body.Device, body.Kind, body.ID, body.Start)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "cast_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleCastControl(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if action == "stop" {
		s.cast.Stop()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var body struct {
		PositionS float64 `json:"position_s"`
	}
	if r.ContentLength != 0 {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	st, err := s.cast.Control(r.Context(), action, body.PositionS)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "cast_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}
