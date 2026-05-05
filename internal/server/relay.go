package server

import (
	"net"

	"github.com/holipay/gametunnel/internal/protocol"
	"github.com/holipay/gametunnel/internal/util"
)

// handleRelay forwards a data packet. For broadcast, it forwards to all
// peers in the room. For unicast, it forwards to the specific peer.
func (s *Server) handleRelay(payload []byte, from *net.UDPAddr) {
	s.mu.RLock()
	sender := s.addrMap[from.String()]
	s.mu.RUnlock()
	if sender == nil {
		return
	}

	// payload is DataPayload: [4B srcIP][4B dstIP][data...]
	if len(payload) < 8 {
		return
	}

	srcIP := net.IP(payload[0:4])
	dstIP := net.IP(payload[4:8])

	// Validate srcIP matches the sender's virtual IP (anti-spoofing)
	if !srcIP.Equal(sender.VirtualIP) {
		return
	}

	encoded := protocol.EncodeChecked(protocol.TypeData, payload)

	// Broadcast
	if util.IsBroadcast(dstIP, s.subnet) {
		s.mu.RLock()
		targets := make([]*net.UDPAddr, 0, len(s.clients))
		for _, c := range s.clients {
			if c.PublicAddr.String() != from.String() {
				targets = append(targets, c.PublicAddr)
			}
		}
		s.mu.RUnlock()

		for _, addr := range targets {
			s.sendCheckedRaw(encoded, addr)
		}
		return
	}

	// Unicast
	s.mu.RLock()
	dst, ok := s.clients[dstIP.String()]
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
	dst, ok := s.clients[dstIP.String()]
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
