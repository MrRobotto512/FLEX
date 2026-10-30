package server

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"

	"tailscale.com/tsnet"
)

// startTailscale joins the tailnet as its own node and serves the same API
// there. LAN traffic is untouched; see docs/remote.md.
func (s *Server) startTailscale(ctx context.Context, handler http.Handler) (*tsnet.Server, error) {
	ts := &tsnet.Server{
		Hostname: s.cfg.TSHostname,
		AuthKey:  s.cfg.TSAuthKey,
		Dir:      filepath.Join(s.cfg.DataDir, "tsnet"),
		Logf:     func(string, ...any) {}, // tsnet is chatty; surface problems via UserLogf
		UserLogf: func(format string, args ...any) { s.log.Info("tsnet: " + fmt.Sprintf(format, args...)) },
	}
	status, err := ts.Up(ctx)
	if err != nil {
		return nil, fmt.Errorf("tsnet up: %w", err)
	}
	ln, err := ts.Listen("tcp", fmt.Sprintf(":%d", s.cfg.HTTPPort))
	if err != nil {
		ts.Close()
		return nil, fmt.Errorf("tsnet listen: %w", err)
	}

	lc, _ := ts.LocalClient()
	identified := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every tailnet request carries a device identity; expose it to handlers.
		if lc != nil {
			if who, err := lc.WhoIs(r.Context(), r.RemoteAddr); err == nil && who.UserProfile != nil {
				r.Header.Set("X-Flex-User", who.UserProfile.LoginName)
				if who.Node != nil {
					r.Header.Set("X-Flex-Device", who.Node.ComputedName)
				}
			}
		}
		handler.ServeHTTP(w, r)
	})
	go http.Serve(ln, identified)

	s.mu.Lock()
	s.tsUp = true
	s.mu.Unlock()
	addr := s.cfg.TSHostname
	if status != nil && status.Self != nil && status.Self.DNSName != "" {
		addr = status.Self.DNSName
	}
	s.log.Info("tailscale listening", "addr", fmt.Sprintf("http://%s:%d", addr, s.cfg.HTTPPort))
	return ts, nil
}
