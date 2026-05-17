package server

import (
	"context"
	"log"
	"net"
	"time"

	"github.com/holipay/gametunnel/internal/i18n"
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
	log.Printf(i18n.T().LogPlayerLeave, c.Username, c.VirtualIP)
	if c.auth == authChallengeSent {
		if s.pendingAuth > 0 {
			s.pendingAuth--
		}
	} else {
		s.markIPFree(c.VirtualIP)
		delete(s.clients, ipKey(c.VirtualIP))
		// Decrement per-IP connection count
		ip := c.PublicAddr.IP.String()
		s.ipConnMu.Lock()
		s.ipConnCount[ip]--
		if s.ipConnCount[ip] <= 0 {
			delete(s.ipConnCount, ip)
		}
		s.ipConnMu.Unlock()
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

	targets := make([]*net.UDPAddr, 0, len(s.clients))
	for _, c := range s.clients {
		targets = append(targets, c.PublicAddr)
	}
	s.mu.RUnlock()

	encoded := s.getEncodedPeerInfo()

	for _, addr := range targets {
		s.sendCheckedRaw(encoded, addr)
	}
}

// sendPeerInfoToClient sends the peer list to a single client.
// The payload includes ALL clients — the client filters out itself locally.
func (s *Server) sendPeerInfoToClient(target *net.UDPAddr) {
	encoded := s.getEncodedPeerInfo()
	s.sendCheckedRaw(encoded, target)
}

// peerInfoCacheTTL is how long cached encoded peer info remains valid.
var peerInfoCacheTTL = peerInfoInterval

// getEncodedPeerInfo returns the encoded PeerInfo packet, using a short-lived
// cache to avoid redundant Marshal+Encode when multiple clients request the
// peer list in quick succession (e.g. after a broadcast triggers re-requests).
func (s *Server) getEncodedPeerInfo() []byte {
	now := time.Now()

	s.peerInfoMu.Lock()
	// Return cached if still fresh
	if s.peerInfoEncoded != nil && now.Sub(s.peerInfoCachedAt) < peerInfoCacheTTL {
		encoded := s.peerInfoEncoded
		s.peerInfoMu.Unlock()
		return encoded
	}

	// Cache miss — rebuild under the same lock to prevent redundant rebuilds
	// by concurrent goroutines (TOCTOU fix).
	s.mu.RLock()
	peers := &protocol.PeerInfoPayload{}
	for _, c := range s.clients {
		peers.Peers = append(peers.Peers, protocol.PeerInfoEntry{
			VirtualIP:  c.VirtualIP,
			PublicAddr: c.PublicAddr,
			Username:   c.Username,
		})
	}
	s.mu.RUnlock()

	encoded := protocol.EncodeChecked(protocol.TypePeerInfo, peers.Marshal())
	s.peerInfoEncoded = encoded
	s.peerInfoCachedAt = now
	s.peerInfoMu.Unlock()

	return encoded
}

// pingInterval is how often the server pings clients for RTT measurement.
const pingInterval = 5 * time.Second

// pingLoop periodically sends TypePing to all authenticated clients
// and tracks timeout (missed pong) for loss rate calculation.
func (s *Server) pingLoop(ctx context.Context) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		now := time.Now()

		s.mu.Lock()
		if len(s.clients) == 0 {
			s.mu.Unlock()
			continue
		}

		// Mark previous pings as missed if no pong received within 2*interval.
		for _, c := range s.clients {
			if !c.lastPingSent.IsZero() && now.Sub(c.lastPingSent) > 2*pingInterval {
				c.pingHistory[c.pingIdx%pingHistorySize] = 0 // missed
				c.pingIdx++
			}
		}

		// Send pings and record sequence/time.
		ts := now.UnixNano()
		ping := &protocol.PingPayload{Timestamp: ts}
		encoded := protocol.EncodeChecked(protocol.TypePing, ping.Marshal())
		for _, c := range s.clients {
			c.pingSeq++
			c.lastPingSent = now
			c.lastPingSeq = c.pingSeq
			s.sendCheckedRaw(encoded, c.PublicAddr)
		}
		s.mu.Unlock()
	}
}

// handlePong processes a latency pong response and updates client RTT,
// jitter, and loss stats.
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
		c.pingHistory[c.pingIdx%pingHistorySize] = rtt
		c.pingIdx++
	}
	s.mu.Unlock()
}
