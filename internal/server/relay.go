package server

import (
	"net"

	"github.com/holipay/gametunnel/internal/protocol"
)

// handleRelay forwards a data packet. For broadcast, it forwards to all
// peers in the room. For unicast, it forwards to the specific peer.
func (s *Server) handleRelay(payload []byte, from *net.UDPAddr) {
	if len(payload) < 8 {
		return
	}

	srcIP := net.IP(payload[0:4])
	dstIP := net.IP(payload[4:8])

	// Single RLock acquisition for sender lookup, validation, and forwarding
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

	// Encode AFTER validation — avoids wasting CPU on spoofed packets
	encoded := protocol.EncodeChecked(protocol.TypeData, payload)

	// Broadcast
	if protocol.IsBroadcast(dstIP, s.subnet) {
		// Use rateKey for zero-allocation comparison
		fromKey := addrToRateKey(from)
		for _, c := range s.clients {
			if addrToRateKey(c.PublicAddr) != fromKey {
				s.sendCheckedRaw(encoded, c.PublicAddr)
			}
		}
		s.mu.RUnlock()
		return
	}

	// Unicast
	dst, ok := s.clients[ip4Key(dstIP)]
	s.mu.RUnlock()
	if !ok {
		return
	}
	s.sendCheckedRaw(encoded, dst.PublicAddr)
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
