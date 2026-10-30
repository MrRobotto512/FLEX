package server

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"time"
)

const discoverMagic = "FLEX_DISCOVER"

// runDiscovery answers LAN broadcasts so clients find the server without
// configuration. `reply` builds the JSON datagram to send back. Blocks until ctx ends.
func runDiscovery(ctx context.Context, port int, reply func() []byte, log *slog.Logger) error {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: port})
	if err != nil {
		return err
	}
	defer conn.Close()
	log.Info("discovery listening", "udp", port)

	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	buf := make([]byte, 512)
	for {
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return err
		}
		if !strings.HasPrefix(string(buf[:n]), discoverMagic) {
			continue
		}
		if _, err := conn.WriteToUDP(reply(), from); err != nil {
			log.Warn("discovery reply", "to", from, "err", err)
		}
	}
}
