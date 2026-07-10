package server

import (
	"github.com/holipay/gametunnel/internal/netkey"
	"context"
	"crypto/hmac"
	"log"
	"net"
	"time"

	"github.com/holipay/gametunnel/internal/auth"
	"github.com/holipay/gametunnel/internal/protocol"
)

func (s *Server) keepaliveLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		s.cleanupStaleClients()
		if s.multiRoom {
			s.cleanupStaleAddrToRoom()
			s.cleanupIdleRooms()
		}
	}
}

// cleanupStaleClients removes clients that haven't sent a keepalive in 30s.
func (s *Server) cleanupStaleClients() {
	s.roomMu.RLock()
	rooms := make([]*Room, 0, len(s.rooms))
	for _, r := range s.rooms {
		rooms = append(rooms, r)
	}
	s.roomMu.RUnlock()

	for _, room := range rooms {
		if room.CleanupStale() {
			room.invalidatePeerInfoCache()
		}
	}
}

// cleanupStaleAddrToRoom removes addrToRoom entries for clients that
// disconnected (room removed from addrMap but addrToRoom was not updated).
func (s *Server) cleanupStaleAddrToRoom() {
	var stale []netkey.RateKey

	// Single pass: scan addrToRoom and check each entry against room.addrMap.
	s.roomMu.RLock()
	for k, room := range s.addrToRoom {
		if room == nil {
			continue
		}
		room.mu.RLock()
		if room.addrMap[k] == nil {
			stale = append(stale, k)
		}
		room.mu.RUnlock()
	}
	s.roomMu.RUnlock()

	if len(stale) == 0 {
		return
	}

	// Re-verify under write lock before deleting (room state may have changed).
	s.roomMu.Lock()
	for _, k := range stale {
		if room := s.addrToRoom[k]; room != nil {
			room.mu.RLock()
			stillStale := room.addrMap[k] == nil
			room.mu.RUnlock()
			if stillStale {
				delete(s.addrToRoom, k)
			}
		}
	}
	s.roomMu.Unlock()
}

// cleanupIdleRooms removes empty rooms that have been idle beyond the timeout.
func (s *Server) cleanupIdleRooms() {
	s.roomMu.Lock()
	now := time.Now()
	for roomID, room := range s.rooms {
		if roomID == "default" {
			continue
		}
		if room.ClientCount() > 0 {
			continue
		}
		lastAct := time.Unix(0, room.lastActivity.Load())
		if now.Sub(lastAct) > roomIdleTimeout {
			room.Stop()
			delete(s.rooms, roomID)
			log.Printf("[room] cleaned up idle room %q (idle for %v)", roomID, now.Sub(lastAct))
		}
	}
	s.roomMu.Unlock()
}

// ── NAT Probe Handler ─────────────────────────────────────────────

// handleNATProbe responds to a client's NAT type probe request.
// The server includes the client's observed external address in the response,
// which the client uses to determine its NAT type.
//
// For multi-probe detection: the client sends multiple probes and compares
// the observed addresses. If they differ, it's a Symmetric NAT.
func (s *Server) handleNATProbe(payload []byte, from *net.UDPAddr) {
	probe, err := protocol.UnmarshalNATProbe(payload)
	if err != nil {
		return
	}

	// Build response with the client's observed address
	resp := &protocol.NATResponsePayload{
		ProbeID:      probe.ProbeID,
		NATType:      protocol.NATTypeUnknown, // client determines this from multiple probes
		ObservedAddr: from,
		AltAddr:      nil, // TODO: respond from alt port for better detection
	}

	s.sendChecked(protocol.TypeNATResponse, resp.Marshal(), from)
}

// ── Rebind Handler (Connection Migration) ───────────────────────

// handleRebind processes a client's address migration request.
// When a client's network changes (WiFi↔4G, NAT rebinding), it sends
// TypeRebind from the new address to reclaim its existing session.
//
// Security: if the room has a password, the client must provide a valid
// HMAC over its virtual IP. Without a password, the server relies on
// virtual IP matching + recent lastSeen (within 60s).
func (s *Server) handleRebind(payload []byte, from *net.UDPAddr) {
	req, err := protocol.UnmarshalRebind(payload)
	if err != nil {
		return
	}
	if req.VirtualIP.To4() == nil {
		s.sendRebindAck(from, false)
		return
	}

	vipKey := netkey.IPKey(req.VirtualIP)

	// Search all rooms for the client with this virtual IP
	var foundRoom *Room
	var foundClient *Client
	var clientAuthRoomID string
	var clientUsername string
	var clientLastSeen time.Time
	var clientPublicAddr *net.UDPAddr

	s.roomMu.RLock()
	for _, room := range s.rooms {
		room.mu.RLock()
		if c, ok := room.clients[vipKey]; ok {
			foundRoom = room
			foundClient = c
			clientAuthRoomID = c.authRoomID
			clientUsername = c.Username
			clientLastSeen = c.GetLastSeen()
			clientPublicAddr = c.PublicAddr
			room.mu.RUnlock()
			break
		}
		room.mu.RUnlock()
	}
	s.roomMu.RUnlock()

	if foundRoom == nil || foundClient == nil {
		s.sendRebindAck(from, false)
		return
	}

	// Verify HMAC if room has a password
	if foundRoom.roomPass != "" {
		if len(req.HMAC) == 0 {
			s.sendRebindAck(from, false)
			return
		}
		key := auth.DeriveKey(foundRoom.roomPass, clientAuthRoomID)
		if key == nil {
			s.sendRebindAck(from, false)
			return
		}
		// Verify HMAC — bind to virtual IP to prevent session hijacking
		virtualAddr := &net.UDPAddr{IP: req.VirtualIP, Port: 0}
		expected := auth.ComputeHMAC(key, nil, clientAuthRoomID, clientUsername, virtualAddr)
		if !hmac.Equal(req.HMAC, expected) {
			s.sendRebindAck(from, false)
			return
		}
	} else {
		// No password — check that the client was recently active.
		// Tightened from 60s to 30s to reduce the session hijacking window.
		if time.Since(clientLastSeen) > 30*time.Second {
			s.sendRebindAck(from, false)
			return
		}
	}

	// Migration valid — update the client's address
	newKey := netkey.AddrToRateKey(from)

	foundRoom.mu.Lock()
	// Re-check that the client is still present (TOCTOU guard).
	if _, stillThere := foundRoom.clients[vipKey]; !stillThere {
		foundRoom.mu.Unlock()
		s.sendRebindAck(from, false)
		return
	}
	// Remove old addrMap entry if client had a prior address (may be nil
	// for restored/persisted clients that never completed registration).
	if clientPublicAddr != nil {
		delete(foundRoom.addrMap, netkey.AddrToRateKey(clientPublicAddr))
	}
	// Update client address
	foundClient.PublicAddr = from
	foundClient.SetLastSeen(time.Now())
	foundRoom.addrMap[newKey] = foundClient
	foundRoom.lastActivity.Store(time.Now().UnixNano())
	foundRoom.mu.Unlock()

	// Update addrToRoom mapping in multi-room mode
	if s.multiRoom {
		s.roomMu.Lock()
		if clientPublicAddr != nil {
			delete(s.addrToRoom, netkey.AddrToRateKey(clientPublicAddr))
		}
		s.addrToRoom[newKey] = foundRoom
		s.roomMu.Unlock()
	}

	log.Printf("[rebind] %s migrated: %v → %s", clientUsername, clientPublicAddr, from)
	s.sendRebindAck(from, true)

	// Send current peer info to the client on new address
	foundRoom.sendPeerInfoToClient(from)
}

func (s *Server) sendRebindAck(to *net.UDPAddr, success bool) {
	ack := &protocol.RebindAckPayload{Success: success}
	s.sendChecked(protocol.TypeRebindAck, ack.Marshal(), to)
}
