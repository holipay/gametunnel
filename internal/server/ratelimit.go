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
	if len(addr.IP) == net.IPv4len {
		// v4-in-v6: 0:0:0:0:0:ffff:a.b.c.d
		k.IP[10] = 0xff
		k.IP[11] = 0xff
		copy(k.IP[12:16], addr.IP)
	} else {
		copy(k.IP[:], addr.IP)
	}
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
// clear the stale buffer to reuse its memory allocation.
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
			// Clear the stale buffer instead of replacing it with a new map.
			// This reuses the existing map memory, avoiding GC pressure from
			// creating a new map every second under high connection counts.
			clear(s.rateBuf[1])
			s.rateMu.Unlock()
		}
	}
}
