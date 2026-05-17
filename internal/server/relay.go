package server

import (
	"net"

	"github.com/holipay/gametunnel/internal/protocol"
)

// maxInlineTargets is the number of peer addresses we can hold on the stack
// without heap allocation. For rooms ≤ 32 players this covers the common case.
const maxInlineTargets = 32

// handleRelay forwards a data packet. For broadcast and multicast, it forwards
// to all peers in the room. For unicast, it forwards to the specific peer.
func (s *Server) handleRelay(payload []byte, from *net.UDPAddr) {
	if len(payload) < 8 {
		return
	}

	srcIP := net.IP(payload[0:4])
	dstIP := net.IP(payload[4:8])

	// Phase 1: validate and collect targets under RLock
	s.mu.RLock()
	sender := s.addrMap[addrToRateKey(from)]
	if sender == nil {
		s.mu.RUnlock()
		return
	}

	// Validate srcIP matches sender's virtual IP (anti-spoofing)
	if !srcIP.Equal(sender.VirtualIP) {
		s.mu.RUnlock()
		return
	}

	isBroadcast := protocol.IsRelayTarget(dstIP, s.subnet)
	fromKey := addrToRateKey(from)

	// Use stack-allocated array for small rooms, fall back to heap for large ones.
	var stackTargets [maxInlineTargets]*net.UDPAddr
	targets := stackTargets[:0]

	if isBroadcast {
		for _, c := range s.clients {
			if addrToRateKey(c.PublicAddr) != fromKey {
				targets = append(targets, c.PublicAddr)
			}
		}
	} else {
		if dst, ok := s.clients[ipKey(dstIP)]; ok {
			targets = append(targets, dst.PublicAddr)
		}
	}
	s.mu.RUnlock()

	// Phase 2: encode and send outside the lock
	if len(targets) == 0 {
		return
	}
	encoded := protocol.EncodeChecked(protocol.TypeData, payload)
	for _, addr := range targets {
		s.sendCheckedRaw(encoded, addr)
	}
}

// handleHolePunch forwards a NAT hole punch packet to the target peer.
//
// The forwarded payload format: [16B srcIP (To16)] [4B srcVirtualIP] [addrStr...]
// The receiver (client handleHolePunchReceived) only reads the first 4 bytes
// as the peer's virtual IP — the srcIP field is unused but kept for debugging.
//
// Uses To16() so both IPv4 and IPv6 client addresses are handled uniformly.
// IPv4 addresses are mapped to v4-in-v6 format (::ffff:x.x.x.x), which is
// 16 bytes and hash-comparable with the client's ipKey() map keys.
func (s *Server) handleHolePunch(payload []byte, from *net.UDPAddr) {
	if len(payload) < 4 {
		return
	}
	dstIP := net.IP(payload[:4])

	srcIP := from.IP.To16()
	if srcIP == nil {
		return
	}

	s.mu.RLock()
	dst, ok := s.clients[ipKey(dstIP)]
	s.mu.RUnlock()

	if !ok {
		return
	}

	addrStr := from.String()
	punchData := make([]byte, 16+len(addrStr))
	copy(punchData[:16], srcIP)
	copy(punchData[16:], []byte(addrStr))
	s.sendChecked(protocol.TypeHolePunch, punchData, dst.PublicAddr)
}
