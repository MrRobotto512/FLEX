package server

import (
	"archive/zip"
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path"
	"strings"
	"sync"
	"testing"
)

// fakeFTP is the part of the console's ftpsrv that installing uses.
type fakeFTP struct {
	mu      sync.Mutex
	dirs    map[string]bool
	files   map[string][]byte
	renamed []string // RNTO targets, in order
}

func startFakeFTP(t *testing.T) *fakeFTP {
	t.Helper()
	f := &fakeFTP{dirs: map[string]bool{"/": true, "/data": true}, files: map[string][]byte{}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	old := ps5FTPPort
	ps5FTPPort = ln.Addr().(*net.TCPAddr).Port
	t.Cleanup(func() { ps5FTPPort = old })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeFTP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	say := func(format string, a ...any) { fmt.Fprintf(c, format+"\r\n", a...) }
	say("220 ftpsrv ready")
	var data net.Listener
	from := ""
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd, arg, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		f.mu.Lock()
		switch cmd {
		case "USER":
			say("331 password please")
		case "PASS":
			say("230 logged in")
		case "TYPE":
			say("200 binary")
		case "MKD":
			if f.dirs[arg] || !f.dirs[path.Dir(arg)] {
				say("550 can't")
			} else {
				f.dirs[arg] = true
				say("257 made")
			}
		case "PASV":
			data, _ = net.Listen("tcp", "127.0.0.1:0")
			p := data.Addr().(*net.TCPAddr).Port
			say("227 Entering Passive Mode (10,0,0,9,%d,%d)", p>>8, p&255) // not the address to use
		case "STOR":
			if data == nil || !f.dirs[path.Dir(arg)] {
				say("553 no such folder")
				break
			}
			say("150 send it")
			f.mu.Unlock()
			dc, err := data.Accept()
			data.Close()
			data = nil
			var body []byte
			if err == nil {
				body, _ = io.ReadAll(dc)
				dc.Close()
			}
			f.mu.Lock()
			f.files[arg] = body
			say("226 stored")
		case "RNFR":
			from = arg
			say("350 go on")
		case "RNTO":
			f.files[arg] = f.files[from]
			delete(f.files, from)
			f.renamed = append(f.renamed, arg)
			say("226 Path renamed") // what ftpsrv answers, not the standard 250
		case "DELE":
			delete(f.files, arg)
			say("250 deleted")
		case "QUIT":
			say("221 bye")
			f.mu.Unlock()
			return
		default:
			say("502 not here")
		}
		f.mu.Unlock()
	}
}

func TestPS5AppInstall(t *testing.T) {
	s := &Server{cfg: Config{DataDir: t.TempDir()}, log: slog.Default()}
	s.ps5up = &ps5Updates{s: s}
	s.ps5 = &ps5Keeper{s: s, hosts: map[string]*ps5Host{}}

	app := map[string]string{
		"eboot.bin":             "SELF app",
		"sce_module/libc.prx":   "prx",
		"sce_sys/param.json":    "{}",
		"sce_sys/pic0.dds":      "DDS selected",
		"assets/DejaVuSans.ttf": "font",
	}
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	for name, body := range app {
		w, _ := zw.Create("PPSA84840/" + name)
		w.Write([]byte(body))
	}
	zw.Close()
	if err := s.StorePS5Client("0.8.1", []byte("\x7fELF"), zb.Bytes()); err != nil {
		t.Fatal(err)
	}

	ftp := startFakeFTP(t)
	ftp.files["/data/homebrew/PPSA84840/eboot.bin"] = []byte("old forwarder") // an earlier tile
	var steps []string
	if err := s.copyPS5App("127.0.0.1", s.ps5up.current(), func(_ float64, msg string) { steps = append(steps, msg) }); err != nil {
		t.Fatal(err)
	}
	ftp.mu.Lock()
	defer ftp.mu.Unlock()
	for name, body := range app {
		if got := string(ftp.files["/data/homebrew/PPSA84840/"+name]); got != body {
			t.Errorf("%s on the console: %q, want %q", name, got, body)
		}
	}
	for name := range ftp.files {
		if strings.HasSuffix(name, ".new") {
			t.Errorf("left behind: %s", name)
		}
	}
	if last := ftp.renamed[len(ftp.renamed)-1]; last != "/data/homebrew/PPSA84840/eboot.bin" {
		t.Errorf("eboot.bin should go last, last was %s", last)
	}
	if len(steps) != len(app) {
		t.Errorf("progress steps: %q", steps)
	}

	// no FTP server: a plain answer, not a hang
	ps5FTPPort = 1
	if err := s.copyPS5App("127.0.0.1", s.ps5up.current(), func(float64, string) {}); err == nil || !strings.Contains(err.Error(), "jailbreak") {
		t.Errorf("without FTP: %v", err)
	}

	// only addresses on the home network
	for _, host := range []string{"8.8.8.8", "ps5.example.com", "127.0.0.1", ""} {
		if err := s.InstallPS5App(host); err == nil {
			t.Errorf("%q was accepted", host)
		}
	}
}
