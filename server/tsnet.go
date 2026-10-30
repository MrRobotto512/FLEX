package server

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"tailscale.com/client/tailscale"
	"tailscale.com/tsnet"
)

// TSInfo is the remote-access state shown in the desktop app and, without
// the sign-in URL, to clients.
type TSInfo struct {
	State   string `json:"state"` // Off, Starting, NeedsLogin, Running, ...
	DNSName string `json:"dns,omitempty"`
	IP      string `json:"ip,omitempty"`
	AuthURL string `json:"-"` // whoever opens it can claim the node: desktop UI only
	Error   string `json:"-"`
}

func (s *Server) TailscaleInfo() TSInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ts
}

func (s *Server) setTS(info TSInfo) {
	s.mu.Lock()
	s.ts = info
	s.mu.Unlock()
}

// startTailscale joins the tailnet as its own node and serves the same API
// there; LAN traffic is untouched (docs/remote.md). Without an auth key the
// node asks for an interactive sign-in once; tsnet keeps the identity in
// DataDir/tsnet afterwards.
func (s *Server) startTailscale(ctx context.Context, handler http.Handler) (*tsnet.Server, error) {
	ts := &tsnet.Server{
		Hostname: s.cfg.TSHostname,
		AuthKey:  s.cfg.TSAuthKey,
		Dir:      filepath.Join(s.cfg.DataDir, "tsnet"),
		Logf:     func(string, ...any) {}, // tsnet is chatty; state is surfaced via TailscaleInfo
		UserLogf: func(format string, args ...any) { s.log.Debug("tsnet: " + fmt.Sprintf(format, args...)) },
	}
	s.setTS(TSInfo{State: "Starting"})
	if err := ts.Start(); err != nil {
		s.setTS(TSInfo{State: "Error", Error: err.Error()})
		return nil, fmt.Errorf("tsnet start: %w", err)
	}
	lc, err := ts.LocalClient()
	if err != nil {
		ts.Close()
		s.setTS(TSInfo{State: "Error", Error: err.Error()})
		return nil, err
	}
	go s.watchTailscale(ctx, ts, lc, handler)
	return ts, nil
}

// watchTailscale mirrors the node's state into TailscaleInfo and starts the
// tailnet listener once the node is up.
func (s *Server) watchTailscale(ctx context.Context, ts *tsnet.Server, lc *tailscale.LocalClient, handler http.Handler) {
	serving := false
	for {
		st, err := lc.StatusWithoutPeers(ctx)
		if err == nil {
			info := TSInfo{State: st.BackendState, AuthURL: st.AuthURL}
			if st.Self != nil {
				info.DNSName = strings.TrimSuffix(st.Self.DNSName, ".")
				for _, ip := range st.Self.TailscaleIPs {
					if ip.Is4() {
						info.IP = ip.String()
						break
					}
				}
			}
			if prev := s.TailscaleInfo(); prev.State != info.State {
				s.log.Info("tailscale", "state", info.State, "dns", info.DNSName, "ip", info.IP)
			}
			s.setTS(info)
			if info.State == "Running" && !serving {
				ln, err := ts.Listen("tcp", fmt.Sprintf(":%d", s.cfg.HTTPPort))
				if err != nil {
					s.log.Error("tailscale listen", "err", err)
				} else {
					serving = true
					go http.Serve(ln, identify(lc, handler))
					s.log.Info("tailscale listening", "addr", fmt.Sprintf("http://%s:%d", info.DNSName, s.cfg.HTTPPort))
				}
			}
		}
		wait := time.Second
		if serving {
			wait = 15 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// identify tags every tailnet request with the caller's Tailscale identity.
func identify(lc *tailscale.LocalClient, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if who, err := lc.WhoIs(r.Context(), r.RemoteAddr); err == nil && who.UserProfile != nil {
			r.Header.Set("X-Flex-User", who.UserProfile.LoginName)
			if who.Node != nil {
				r.Header.Set("X-Flex-Device", who.Node.ComputedName)
			}
		}
		next.ServeHTTP(w, r)
	})
}
