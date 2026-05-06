package server

import (
	"log"
	"net"
	"time"

	"github.com/holipay/gametunnel/internal/protocol"
)

// handleKeepAlive updates the last-seen time for a client.
func (s *Server) handleKeepAlive(from *net.UDPAddr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.addrMap[from.String()]; c != nil {
		c.LastSeen = time.Now()
	}
}

// handleDisconnect removes a client that is gracefully disconnecting.
func (s *Server) handleDisconnect(from *net.UDPAddr) {
	s.mu.Lock()
	c := s.addrMap[from.String()]
	if c == nil {
		s.mu.Unlock()
		return
	}
	log.Printf("[-] %s (%s) 主动断开", c.Username, c.VirtualIP)
	s.markIPFree(c.VirtualIP)
	delete(s.clients, c.VirtualIP.String())
	delete(s.addrMap, from.String())
	s.mu.Unlock()

	s.sendPeerInfoTo(nil, nil, nil)
}

// handlePeerRequest handles a client's request for the peer list.
func (s *Server) handlePeerRequest(from *net.UDPAddr) {
	s.mu.RLock()
	c := s.addrMap[from.String()]
	s.mu.RUnlock()

	if c == nil {
		return
	}

	s.sendPeerInfoTo([]*net.UDPAddr{from}, nil, c.VirtualIP)
}

// sendPeerInfoTo sends peer information to the specified targets.
// If targets is nil, broadcasts to all clients (excluding exclude).
// If selfIP is set, excludes that IP from the peer list.
func (s *Server) sendPeerInfoTo(targets []*net.UDPAddr, exclude *net.UDPAddr, selfIP net.IP) {
	s.mu.RLock()
	snapshot := make([]peerSnapshot, 0, len(s.clients))
	for _, c := range s.clients {
		snapshot = append(snapshot, peerSnapshot{
			virtualIP:  c.VirtualIP,
			publicAddr: c.PublicAddr,
			username:   c.Username,
		})
	}
	s.mu.RUnlock()

	peers := &protocol.PeerInfoPayload{}
	for _, sn := range snapshot {
		if selfIP != nil && sn.virtualIP.Equal(selfIP) {
			continue
		}
		peers.Peers = append(peers.Peers, protocol.PeerInfoEntry{
			VirtualIP:  sn.virtualIP,
			PublicAddr: sn.publicAddr,
			Username:   sn.username,
		})
	}

	encoded := protocol.EncodeChecked(protocol.TypePeerInfo, peers.Marshal())

	if targets != nil {
		for _, addr := range targets {
			s.sendCheckedRaw(encoded, addr)
		}
		return
	}

	for _, sn := range snapshot {
		if exclude != nil && sn.publicAddr.String() == exclude.String() {
			continue
		}
		s.sendCheckedRaw(encoded, sn.publicAddr)
	}
}
