package server

import (
	"context"
	"net"
	"time"
)

// ── Packet Rate Limiting ───────────────────────────────────────

const (
	rateLimit    = 500 // max packets per window per client
	rateInterval = time.Second
)

// rateKey is a fixed-size key for rate limiting, avoiding string allocation per packet.
// Uses 16-byte IP to support both IPv4 (as v4-in-v6 mapped) and IPv6 addresses.
type rateKey struct {
	IP   [16]byte
	Port uint16
}

func addrToRateKey(addr *net.UDPAddr) rateKey {
	var k rateKey
	copy(k.IP[:], addr.IP.To16())
	k.Port = uint16(addr.Port)
	return k
}

// checkRate returns true if the address has not exceeded the packet rate limit.
func (s *Server) checkRate(addr *net.UDPAddr) bool {
	key := addrToRateKey(addr)
	s.rateMu.Lock()
	s.rateBuf[0][key]++
	ok := s.rateBuf[0][key] <= rateLimit
	s.rateMu.Unlock()
	return ok
}

// rateLimitLoop resets the per-client packet counter every second
// using a double-buffer swap: swap pointers under lock (O(1)), then
// clear the stale buffer outside the lock to avoid contention.
func (s *Server) rateLimitLoop(ctx context.Context) {
	s.rateTick = time.NewTicker(rateInterval)
	defer s.rateTick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.rateTick.C:
			s.rateMu.Lock()
			s.rateBuf[0], s.rateBuf[1] = s.rateBuf[1], s.rateBuf[0]
			// Clear stale buffer under lock to prevent race with checkRate.
			// The tick interval is 1s; map clear is fast; contention is negligible.
			for k := range s.rateBuf[1] {
				delete(s.rateBuf[1], k)
			}
			s.rateMu.Unlock()
		}
	}
}
