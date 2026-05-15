package server

import (
	"net"

	"github.com/holipay/gametunnel-protocol/protocol"
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
		if dst, ok := s.clients[ip4Key(dstIP)]; ok {
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
func (s *Server) handleHolePunch(payload []byte, from *net.UDPAddr) {
	if len(payload) < 4 {
		return
	}
	dstIP := net.IP(payload[:4])

	srcIP4 := from.IP.To4()
	if srcIP4 == nil {
		return
	}

	s.mu.RLock()
	dst, ok := s.clients[ip4Key(dstIP)]
	s.mu.RUnlock()

	if !ok {
		return
	}

	addrStr := from.String()
	punchData := make([]byte, 4+len(addrStr))
	copy(punchData[:4], srcIP4)
	copy(punchData[4:], []byte(addrStr))
	s.sendChecked(protocol.TypeHolePunch, punchData, dst.PublicAddr)
}
