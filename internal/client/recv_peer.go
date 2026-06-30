package client

import (
	"github.com/holipay/gametunnel/internal/netkey"
	"context"
	"log"
	"net"
	"time"

	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/protocol"
)

// handleDirectHolePunch processes a TypeHolePunch received directly from a peer.
// Confirms direct reachability and triggers a punch-back response.
func (t *Tunnel) handleDirectHolePunch(ctx context.Context, from *net.UDPAddr, msg *protocol.Message) {
	if len(msg.Payload) < 4 {
		return
	}
	peerIP := net.IP(append([]byte(nil), msg.Payload[:4]...))

	t.mu.RLock()
	peer, ok := t.peers[netkey.IPKey(peerIP)]
	if !ok || peer.PublicAddr.Load() == nil {
		t.mu.RUnlock()
		return
	}
	peerAddr := peer.PublicAddr.Load()
	t.mu.RUnlock()

	// Verify the sender matches the peer's known public address (anti-spoofing)
	if !from.IP.Equal(peerAddr.IP) || from.Port != peerAddr.Port {
		return
	}

	// Rate limit: at most once per holePunchBackoff per peer
	if !peer.tryRateLimitHolePunch(holePunchBackoff) {
		return
	}

	// Mark direct path confirmed — received a packet directly from the peer
	peer.DirectReach.Store(true)

	// Punch back in a goroutine — don't block the receive loop
	t.holePunchWg.Add(1)
	go func() {
		defer t.holePunchWg.Done()
		t.burstHolePunch(peerAddr, holePunchBurstPerPhase, 50*time.Millisecond, ctx)
	}()
}

// handlePeerInfo updates the peer list from the server.
func (t *Tunnel) handlePeerInfo(ctx context.Context, payload []byte) {
	info, err := protocol.UnmarshalPeerInfo(payload)
	if err != nil {
		return
	}

	var newPeerIPs []net.IP // peers that need hole punching
	var changedPeerIPs []net.IP // peers whose public address changed (need re-punch)
	now := time.Now()

	t.mu.Lock()

	// Build a fresh map instead of clearing t.peers in-place.
	// oldPeers and t.peers MUST be different maps so that looking
	// up existing peers in oldPeers works correctly.
	oldPeers := t.peers
	if oldPeers == nil {
		oldPeers = make(map[[16]byte]*Peer)
	}
	t.peers = make(map[[16]byte]*Peer, len(info.Peers))
	for _, entry := range info.Peers {
		// Skip self — server sends full list including this client
		if entry.VirtualIP.Equal(t.session.virtualIP) {
			continue
		}
		key := netkey.IPKey(entry.VirtualIP)
		// Normalize PublicAddr.IP to 16 bytes (IPv4 → ::ffff:x.x.x.x) so
		// that IP comparisons with addresses received on the IPv6 socket
		// (always 16 bytes) work correctly. Required for fromServer check
		// in receiveFromServer and P2P detection in handleDirectData.
		pubAddr := entry.PublicAddr
		if pubAddr != nil {
			if ip16 := pubAddr.IP.To16(); ip16 != nil {
				pubAddr = &net.UDPAddr{IP: ip16, Port: pubAddr.Port}
			}
		}
		if existing, ok := oldPeers[key]; ok {
			// Check if peer's public address changed (NAT rebinding)
			existingAddr := existing.PublicAddr.Load()
			addrChanged := existingAddr != nil && pubAddr != nil &&
				(!existingAddr.IP.Equal(pubAddr.IP) || existingAddr.Port != pubAddr.Port)
			if addrChanged {
				log.Printf(i18n.T().LogPeerAddrChange, entry.Username, entry.VirtualIP, existing.PublicAddr.Load(), entry.PublicAddr)
				existing.DirectReach.Store(false) // reset P2P status, need re-punch
				changedPeerIPs = append(changedPeerIPs, entry.VirtualIP)
			}
			existing.PublicAddr.Store(pubAddr)
			existing.Username = entry.Username
			existing.lastSeen.Store(now.UnixNano())
			t.peers[key] = existing
		} else {
			p := &Peer{
				VirtualIP: entry.VirtualIP,
				Username:  entry.Username,
			}
			if pubAddr != nil {
				p.PublicAddr.Store(pubAddr)
			}
			p.lastSeen.Store(now.UnixNano())
			t.peers[key] = p
			log.Printf(i18n.T().LogNewPeer, entry.Username, entry.VirtualIP)
			newPeerIPs = append(newPeerIPs, entry.VirtualIP)
		}
	}
	// Log removed peers (those in oldPeers but not in the updated t.peers)
	for key, peer := range oldPeers {
		if _, ok := t.peers[key]; !ok {
			log.Printf(i18n.T().LogPeerLeave2, peer.Username, peer.VirtualIP)
		}
	}

	t.mu.Unlock()

	// Launch hole punches outside the lock to avoid holding it during goroutine creation
	allPeerIPs := append(newPeerIPs, changedPeerIPs...)
	for _, peerIP := range allPeerIPs {
		go t.startHolePunch(ctx, peerIP)
		t.sendHolePunchRelay(peerIP)
	}
}
