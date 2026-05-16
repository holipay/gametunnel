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
			s.rateMu.Unlock()
			// Clear stale buffer outside the lock — no contention with checkRate.
			for k := range s.rateBuf[1] {
				delete(s.rateBuf[1], k)
			}
		}
	}
}

// ── Registration Rate Limiting ─────────────────────────────────

// checkRegRate returns true if the IP has not exceeded the registration rate limit.
func (s *Server) checkRegRate(ip string) bool {
	s.regMu.Lock()
	s.regBuf[0][ip]++
	ok := s.regBuf[0][ip] <= s.maxRegPerIP
	s.regMu.Unlock()
	return ok
}

// regRateLimitLoop resets the per-IP registration counter every second
// using a double-buffer swap.
func (s *Server) regRateLimitLoop(ctx context.Context) {
	s.regTick = time.NewTicker(time.Second)
	defer s.regTick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.regTick.C:
			s.regMu.Lock()
			s.regBuf[0], s.regBuf[1] = s.regBuf[1], s.regBuf[0]
			s.regMu.Unlock()
			for k := range s.regBuf[1] {
				delete(s.regBuf[1], k)
			}
		}
	}
}
