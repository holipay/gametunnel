package server

import (
	"context"
	"log"
	"net"
	"time"

	"github.com/holipay/gametunnel/internal/protocol"
)

const (
	// peerInfoInterval is how often the batch PeerInfo broadcast runs.
	// 50ms coalesces up to ~20 join/leave events per broadcast, acceptable latency for LAN games.
	peerInfoInterval = 50 * time.Millisecond
)

// handleKeepAlive updates the last-seen time for a client.
func (s *Server) handleKeepAlive(from *net.UDPAddr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.addrMap[addrToRateKey(from)]; c != nil {
		c.LastSeen = time.Now()
	}
}

// handleDisconnect removes a client that is gracefully disconnecting.
func (s *Server) handleDisconnect(from *net.UDPAddr) {
	fromKey := addrToRateKey(from)
	s.mu.Lock()
	c := s.addrMap[fromKey]
	if c == nil {
		s.mu.Unlock()
		return
	}
	log.Printf("[-] %s (%s) 主动断开", c.Username, c.VirtualIP)
	if c.auth == authChallengeSent {
		s.pendingAuth--
	} else {
		s.markIPFree(c.VirtualIP)
		delete(s.clients, ip4Key(c.VirtualIP))
	}
	delete(s.addrMap, fromKey)
	s.mu.Unlock()

	s.peerInfoDirty.Store(true)
}

// handlePeerRequest handles a client's request for the peer list.
// Responds immediately (not batched) since this is an on-demand request.
func (s *Server) handlePeerRequest(from *net.UDPAddr) {
	s.mu.RLock()
	c := s.addrMap[addrToRateKey(from)]
	s.mu.RUnlock()

	if c == nil {
		return
	}

	s.sendPeerInfoToClient(from)
}

// peerInfoLoop periodically checks the dirty flag and broadcasts PeerInfo.
// This coalesces rapid join/leave events into a single broadcast per interval.
func (s *Server) peerInfoLoop(ctx context.Context) {
	ticker := time.NewTicker(peerInfoInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if s.peerInfoDirty.CompareAndSwap(true, false) {
			s.sendPeerInfoBroadcast()
		}
	}
}

// sendPeerInfoBroadcast sends the full peer list to all connected clients.
// The payload includes ALL clients — each client filters out itself locally.
func (s *Server) sendPeerInfoBroadcast() {
	s.mu.RLock()
	if len(s.clients) == 0 {
		s.mu.RUnlock()
		return
	}

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
		peers.Peers = append(peers.Peers, protocol.PeerInfoEntry{
			VirtualIP:  sn.virtualIP,
			PublicAddr: sn.publicAddr,
			Username:   sn.username,
		})
	}

	encoded := protocol.EncodeChecked(protocol.TypePeerInfo, peers.Marshal())

	for _, sn := range snapshot {
		s.sendCheckedRaw(encoded, sn.publicAddr)
	}
}

// sendPeerInfoToClient sends the peer list to a single client.
// The payload includes ALL clients — the client filters out itself locally.
func (s *Server) sendPeerInfoToClient(target *net.UDPAddr) {
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
		peers.Peers = append(peers.Peers, protocol.PeerInfoEntry{
			VirtualIP:  sn.virtualIP,
			PublicAddr: sn.publicAddr,
			Username:   sn.username,
		})
	}

	encoded := protocol.EncodeChecked(protocol.TypePeerInfo, peers.Marshal())
	s.sendCheckedRaw(encoded, target)
}

// pingInterval is how often the server pings clients for RTT measurement.
const pingInterval = 5 * time.Second

// pingLoop periodically sends TypePing to all authenticated clients.
func (s *Server) pingLoop(ctx context.Context) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		s.mu.RLock()
		if len(s.clients) == 0 {
			s.mu.RUnlock()
			continue
		}
		snapshot := make([]peerSnapshot, 0, len(s.clients))
		for _, c := range s.clients {
			snapshot = append(snapshot, peerSnapshot{
				publicAddr: c.PublicAddr,
			})
		}
		s.mu.RUnlock()

		ts := time.Now().UnixNano()
		ping := &protocol.PingPayload{Timestamp: ts}
		encoded := protocol.EncodeChecked(protocol.TypePing, ping.Marshal())
		for _, sn := range snapshot {
			s.sendCheckedRaw(encoded, sn.publicAddr)
		}
	}
}

// handlePong processes a latency pong response and updates client RTT.
func (s *Server) handlePong(payload []byte, from *net.UDPAddr) {
	ping, err := protocol.UnmarshalPing(payload)
	if err != nil {
		return
	}

	rtt := time.Since(time.Unix(0, ping.Timestamp))
	if rtt < 0 || rtt > 10*time.Second {
		return
	}

	s.mu.Lock()
	if c := s.addrMap[addrToRateKey(from)]; c != nil {
		c.RTT = rtt
	}
	s.mu.Unlock()
}
