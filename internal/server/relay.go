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

	// Use stack-allocated array for small rooms, fall back to heap for large ones.
	var stackTargets [maxInlineTargets]*net.UDPAddr
	targets := stackTargets[:0]

	if isBroadcast {
		for _, c := range s.clients {
			if c != sender {
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
	packetSize := len(encoded)
	for _, addr := range targets {
		if s.bwLimiter.Allow(addr, packetSize) {
			s.sendCheckedRaw(encoded, addr)
		}
	}
	s.totalPacketsRelay.Add(1)
}

// handleHolePunch forwards a NAT hole punch packet to the target peer.
//
// Forwarded payload: [4B srcVirtualIP] [addrStr...]
// The receiver (client handleHolePunchReceived) reads the first 4 bytes as
// the peer's virtual IP to look up in its local peers map. The addrStr is
// used for debugging/logging only — the actual punch delivery uses the
// peer's PublicAddr from the PeerInfo message.
func (s *Server) handleHolePunch(payload []byte, from *net.UDPAddr) {
	if len(payload) < 4 {
		return
	}
	dstIP := net.IP(payload[:4])

	// Look up the source client by their public address to get their virtual IP.
	s.mu.RLock()
	src, ok1 := s.addrMap[addrToRateKey(from)]
	dst, ok2 := s.clients[ipKey(dstIP)]
	s.mu.RUnlock()

	if !ok1 || !ok2 {
		return
	}

	addrStr := from.String()
	punchData := make([]byte, 4+len(addrStr))
	copy(punchData[:4], src.VirtualIP.To4())
	copy(punchData[4:], []byte(addrStr))
	s.sendChecked(protocol.TypeHolePunch, punchData, dst.PublicAddr)
}
