package server

// Installing FLEX's PS5 app onto a console from this PC. The jailbreak's FTP
// server (ftpsrv, port 2121) opens the console's file system to the network;
// FLEX copies the app build it keeps for the consoles' self-updates into
// /data/homebrew/<title ID>/, where Shadow Mount+ picks it up and puts the
// FLEX tile on the home screen. That makes the first install a button in the
// FLEX window instead of copying files by hand, and it is how new home-screen
// pictures (sce_sys) reach a console: the app's own updates leave sce_sys
// alone, since the home screen reads it when the tile is set up.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/textproto"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ps5FTPPort = 2121 // ftpsrv's port (a variable for the tests)

// PS5Console is a console this FLEX knows, as the desktop window shows it.
type PS5Console struct {
	Host    string  `json:"host"`
	FTP     bool    `json:"ftp"`     // its FTP server answered the last look
	Checked bool    `json:"checked"` // FTP has been looked at
	Busy    bool    `json:"busy"`    // an install is running
	Done    float64 `json:"done"`    // 0..1 of that install
	Message string  `json:"message,omitempty"`
	Failed  bool    `json:"failed,omitempty"`
}

// PS5Consoles lists the consoles that have used FLEX (or that an install
// was started for), each with whether its FTP server is up. FTP is looked at
// in the background, at most every 10 seconds.
func (s *Server) PS5Consoles() []PS5Console {
	k := s.ps5
	k.mu.Lock()
	defer k.mu.Unlock()
	hosts := k.list()
	if !k.probing && time.Since(k.probed) > 10*time.Second {
		k.probing = true
		go k.probeFTP(hosts)
	}
	out := make([]PS5Console, 0, len(hosts))
	for _, h := range hosts {
		st := k.hosts[h]
		out = append(out, PS5Console{Host: h, FTP: st.ftp, Checked: st.ftpChecked, Busy: st.busy,
			Done: st.done, Message: st.msg, Failed: st.failed})
	}
	return out
}

func (k *ps5Keeper) probeFTP(hosts []string) {
	up := make([]bool, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			up[i] = portOpen(h, ps5FTPPort)
		}()
	}
	wg.Wait()
	k.mu.Lock()
	defer k.mu.Unlock()
	for i, h := range hosts {
		if st := k.hosts[h]; st != nil {
			st.ftp, st.ftpChecked = up[i], true
		}
	}
	k.probing, k.probed = false, time.Now()
}

// InstallPS5App starts copying the app onto the console at host (an address
// on the home network); PS5Consoles shows how it goes.
func (s *Server) InstallPS5App(host string) error {
	addr, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil || !addr.Unmap().IsPrivate() {
		return fmt.Errorf("%q isn't an address on the home network: give the PS5's, like 192.168.0.176 (on the PS5: Settings > Network > Connection Status)", host)
	}
	host = addr.Unmap().String()
	cl := s.ps5up.current()
	if cl == nil || cl.App == nil {
		return errors.New("this FLEX hasn't downloaded the PS5 app yet; it comes with FLEX's check for updates, a minute after FLEX starts with internet")
	}
	k := s.ps5
	k.mu.Lock()
	st, known := k.hosts[host]
	if !known {
		st = &ps5Host{}
		k.hosts[host] = st
	}
	if st.busy {
		k.mu.Unlock()
		return errors.New("FLEX is already installing on that PS5")
	}
	st.busy, st.done, st.msg, st.failed = true, 0, "Connecting…", false
	list := k.list()
	k.mu.Unlock()
	if !known {
		k.save(list)
	}
	go func() {
		err := s.copyPS5App(host, cl, func(done float64, msg string) {
			k.update(host, func(h *ps5Host) { h.done, h.msg = done, msg })
		})
		k.update(host, func(h *ps5Host) {
			h.busy = false
			if err != nil {
				h.failed, h.msg = true, err.Error()
				return
			}
			h.done, h.ftp, h.ftpChecked = 1, true, true
			h.msg = fmt.Sprintf("FLEX %s is installed. The FLEX tile appears on the home screen within about 15 seconds; if it doesn't, open Shadow Mount+ once.", cl.Version)
		})
		if err != nil {
			s.log.Warn("ps5: install failed", "host", host, "err", err)
		} else {
			s.log.Info("ps5: app installed", "host", host, "version", cl.Version, "title", cl.App.TitleID)
		}
	}()
	return nil
}

// save keeps the list of consoles for the next start.
func (k *ps5Keeper) save(hosts []string) {
	b, _ := json.Marshal(hosts)
	k.s.db.SetSetting("ps5_hosts", string(b))
}

// copyPS5App puts every file of the app build into
// /data/homebrew/<title ID>/ on the console, eboot.bin last. Each file goes
// up under a temporary name first and is then renamed over the old one, so
// a dropped connection never leaves half a file (and an open FLEX doesn't
// stop its own eboot.bin from being replaced).
func (s *Server) copyPS5App(host string, cl *PS5Client, progress func(float64, string)) error {
	c, err := dialFTP(host)
	if err != nil {
		return fmt.Errorf("the PS5's FTP server (port %d) isn't answering: run the jailbreak again (it starts FTP), then try again (%v)", ps5FTPPort, err)
	}
	defer c.quit()
	files := append([]PS5AppFile(nil), cl.App.Files...)
	sort.SliceStable(files, func(i, j int) bool { return files[j].Path == "eboot.bin" && files[i].Path != "eboot.bin" })
	total := 0
	dirs := map[string]bool{}
	for _, f := range files {
		total += f.Size
		dirs[path.Dir(f.Path)] = true
	}
	base := "/data/homebrew/" + cl.App.TitleID
	c.mkdirAll(base)
	for d := range dirs {
		if d != "." {
			c.mkdirAll(base + "/" + d)
		}
	}
	sent := 0
	for _, f := range files {
		progress(float64(sent)/float64(max(total, 1)), "Copying "+f.Path+"…")
		data, err := os.ReadFile(filepath.Join(s.ps5up.dir(), "app", filepath.FromSlash(f.Path)))
		if err != nil || sha(data) != f.SHA256 {
			return fmt.Errorf("this FLEX's copy of the app is incomplete (%s); it is downloaded again with the next update", f.Path)
		}
		if err := c.put(base+"/"+f.Path, data); err != nil {
			return fmt.Errorf("copying %s to the PS5: %v", f.Path, err)
		}
		sent += len(data)
	}
	return nil
}

// ftpConn is the little of FTP that installing needs.
type ftpConn struct {
	host string
	conn net.Conn
	tp   *textproto.Conn
}

func dialFTP(host string) (*ftpConn, error) {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(ps5FTPPort)), 5*time.Second)
	if err != nil {
		return nil, err
	}
	c := &ftpConn{host: host, conn: conn, tp: textproto.NewConn(conn)}
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	if code, msg, err := c.tp.ReadResponse(0); err != nil || code != 220 {
		conn.Close()
		return nil, ftpErr("greeting", code, msg, err)
	}
	code, msg, err := c.cmd("USER anonymous")
	if err == nil && code == 331 {
		code, msg, err = c.cmd("PASS flex")
	}
	if err != nil || code != 230 {
		conn.Close()
		return nil, ftpErr("login", code, msg, err)
	}
	if code, msg, err := c.cmd("TYPE I"); err != nil || code != 200 {
		conn.Close()
		return nil, ftpErr("TYPE I", code, msg, err)
	}
	return c, nil
}

func ftpErr(what string, code int, msg string, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return fmt.Errorf("%s: the PS5 answered %d %s", what, code, msg)
}

func (c *ftpConn) cmd(format string, args ...any) (int, string, error) {
	c.conn.SetDeadline(time.Now().Add(30 * time.Second))
	if err := c.tp.PrintfLine(format, args...); err != nil {
		return 0, "", err
	}
	return c.tp.ReadResponse(0)
}

func (c *ftpConn) quit() {
	c.cmd("QUIT")
	c.conn.Close()
}

// mkdirAll makes every folder of p that is missing (an existing one answers
// with an error, which is fine; a missing one shows up when storing fails).
func (c *ftpConn) mkdirAll(p string) {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	for i := range parts {
		c.cmd("MKD /%s", strings.Join(parts[:i+1], "/"))
	}
}

var pasvRe = regexp.MustCompile(`(\d+),(\d+),(\d+),(\d+),(\d+),(\d+)`)

// passive opens a data connection. The console's own address is used rather
// than the one in its answer, which can be another of its interfaces.
func (c *ftpConn) passive() (net.Conn, error) {
	code, msg, err := c.cmd("PASV")
	if err != nil || code != 227 {
		return nil, ftpErr("PASV", code, msg, err)
	}
	m := pasvRe.FindStringSubmatch(msg)
	if m == nil {
		return nil, fmt.Errorf("PASV: can't read %q", msg)
	}
	hi, _ := strconv.Atoi(m[5])
	lo, _ := strconv.Atoi(m[6])
	return net.DialTimeout("tcp", net.JoinHostPort(c.host, strconv.Itoa(hi<<8|lo)), 5*time.Second)
}

func (c *ftpConn) store(p string, data []byte) error {
	dc, err := c.passive()
	if err != nil {
		return err
	}
	code, msg, err := c.cmd("STOR %s", p)
	if err != nil || (code != 150 && code != 125) {
		dc.Close()
		return ftpErr("STOR "+p, code, msg, err)
	}
	dc.SetDeadline(time.Now().Add(2 * time.Minute))
	_, werr := dc.Write(data)
	cerr := dc.Close()
	c.conn.SetDeadline(time.Now().Add(2 * time.Minute))
	code, msg, err = c.tp.ReadResponse(0)
	switch {
	case werr != nil:
		return werr
	case cerr != nil:
		return cerr
	case err != nil || code/100 != 2:
		return ftpErr("STOR "+p, code, msg, err)
	}
	return nil
}

func (c *ftpConn) rename(from, to string) error {
	code, msg, err := c.cmd("RNFR %s", from)
	if err != nil || code != 350 {
		return ftpErr("RNFR", code, msg, err)
	}
	if code, msg, err = c.cmd("RNTO %s", to); err != nil || code/100 != 2 { // ftpsrv says 226, the standard 250
		return ftpErr("RNTO", code, msg, err)
	}
	return nil
}

// put stores data at p by way of p.new.
func (c *ftpConn) put(p string, data []byte) error {
	tmp := p + ".new"
	if err := c.store(tmp, data); err != nil {
		return err
	}
	if c.rename(tmp, p) == nil {
		return nil
	}
	// a server that won't rename over a file: remove the old one first
	c.cmd("DELE %s", p)
	if err := c.rename(tmp, p); err != nil {
		c.cmd("DELE %s", tmp)
		if path.Base(p) == "eboot.bin" {
			return fmt.Errorf("%v (if FLEX is open on the PS5, close it and try again)", err)
		}
		return err
	}
	return nil
}
