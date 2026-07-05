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
	var peerIPBuf [4]byte
	copy(peerIPBuf[:], msg.Payload[:4])
	peerIP := net.IP(peerIPBuf[:])

	t.mu.RLock()
	peers := t.peerSnapshot.Load().(map[[16]byte]*Peer)
	peer, ok := peers[netkey.IPKey(peerIP)]
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

	var newPeerIPs []net.IP   // peers that need hole punching
	var changedPeerIPs []net.IP // peers whose public address changed (need re-punch)
	now := time.Now()

	// Collect log messages, emit after update to avoid blocking readers.
	type peerLog struct {
		format string
		args   []interface{}
	}
	var logs []peerLog

	// Copy-on-write: load current map, create new map with updates, store atomically.
	oldPeers := t.peers
	newPeers := make(map[[16]byte]*Peer, len(oldPeers))

	// Copy all existing peers (mark as stale)
	for key, peer := range oldPeers {
		peer.stale = true
		newPeers[key] = peer
	}

	// Update existing peers or add new ones
	for _, entry := range info.Peers {
		if entry.VirtualIP.Equal(t.session.virtualIP) {
			continue
		}
		key := netkey.IPKey(entry.VirtualIP)
		pubAddr := entry.PublicAddr
		if pubAddr != nil {
			if ip16 := pubAddr.IP.To16(); ip16 != nil {
				pubAddr = &net.UDPAddr{IP: ip16, Port: pubAddr.Port}
			}
		}
		if existing, ok := newPeers[key]; ok {
			existing.stale = false
			existingAddr := existing.PublicAddr.Load()
			addrChanged := existingAddr != nil && pubAddr != nil &&
				(!existingAddr.IP.Equal(pubAddr.IP) || existingAddr.Port != pubAddr.Port)
			if addrChanged {
				logs = append(logs, peerLog{i18n.T().LogPeerAddrChange, []interface{}{entry.Username, entry.VirtualIP, existing.PublicAddr.Load(), entry.PublicAddr}})
				existing.DirectReach.Store(false)
				changedPeerIPs = append(changedPeerIPs, entry.VirtualIP)
			}
			existing.PublicAddr.Store(pubAddr)
			existing.Username = entry.Username
			existing.NATType = entry.NATType
			existing.lastSeen.Store(now.UnixNano())
		} else {
			p := &Peer{
				VirtualIP: entry.VirtualIP,
				Username:  entry.Username,
				NATType:   entry.NATType,
			}
			if pubAddr != nil {
				p.PublicAddr.Store(pubAddr)
			}
			p.lastSeen.Store(now.UnixNano())
			newPeers[key] = p
			logs = append(logs, peerLog{i18n.T().LogNewPeer, []interface{}{entry.Username, entry.VirtualIP}})
			newPeerIPs = append(newPeerIPs, entry.VirtualIP)
		}
	}

	// Remove stale peers
	for key, peer := range newPeers {
		if peer.stale {
			logs = append(logs, peerLog{i18n.T().LogPeerLeave2, []interface{}{peer.Username, peer.VirtualIP}})
			delete(newPeers, key)
		}
	}

	// Update authoritative map and atomic snapshot
	t.peers = newPeers
	t.peerSnapshot.Store(newPeers)

	// Emit log messages
	for _, l := range logs {
		log.Printf(l.format, l.args...)
	}

	// Launch hole punches
	allPeerIPs := append(newPeerIPs, changedPeerIPs...)
	for _, peerIP := range allPeerIPs {
		t.holePunchWg.Add(1)
		go func(ip net.IP) {
			defer t.holePunchWg.Done()
			t.startHolePunch(ctx, ip)
		}(peerIP)
		t.sendHolePunchRelay(peerIP)
	}
}


