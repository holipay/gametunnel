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
type rateKey struct {
	IP   [4]byte
	Port uint16
}

func addrToRateKey(addr *net.UDPAddr) rateKey {
	var k rateKey
	copy(k.IP[:], addr.IP.To4())
	k.Port = uint16(addr.Port)
	return k
}

// checkRate returns true if the address has not exceeded the packet rate limit.
// Uses the active rate counter map (rateCount). The alternate map (rateCountAlt)
// is swapped in every interval by rateLimitLoop.
func (s *Server) checkRate(addr *net.UDPAddr) bool {
	key := addrToRateKey(addr)
	s.rateMu.Lock()
	s.rateCount[key]++
	ok := s.rateCount[key] <= rateLimit
	s.rateMu.Unlock()
	return ok
}

// rateLimitLoop swaps the rate counter map every second.
// Double-buffer approach: swap active/alt maps instead of iterating to delete.
func (s *Server) rateLimitLoop(ctx context.Context) {
	s.rateTick = time.NewTicker(rateInterval)
	defer s.rateTick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.rateTick.C:
			s.rateMu.Lock()
			// Swap: old active becomes new alt (to be cleared next cycle),
			// old alt becomes new active (already empty from last swap).
			s.rateCount, s.rateCountAlt = s.rateCountAlt, s.rateCount
			// Clear the new alt (was the old active) for next swap.
			for k := range s.rateCountAlt {
				delete(s.rateCountAlt, k)
			}
			s.rateMu.Unlock()
		}
	}
}

// ── Registration Rate Limiting ─────────────────────────────────

// checkRegRate returns true if the IP has not exceeded the registration rate limit.
func (s *Server) checkRegRate(ip string) bool {
	s.regMu.Lock()
	s.regCount[ip]++
	ok := s.regCount[ip] <= s.maxRegPerIP
	s.regMu.Unlock()
	return ok
}

// regRateLimitLoop resets the per-IP registration counter every second.
func (s *Server) regRateLimitLoop(ctx context.Context) {
	s.regTick = time.NewTicker(time.Second)
	defer s.regTick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.regTick.C:
			s.regMu.Lock()
			for k := range s.regCount {
				delete(s.regCount, k)
			}
			s.regMu.Unlock()
		}
	}
}
