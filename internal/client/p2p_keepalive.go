package client

import (
	"context"
	"net"
	"time"
)

// p2pKeepaliveInterval is how often we send a keepalive packet to peers
// with confirmed DirectReach. Keeps NAT mappings alive so the P2P path
// doesn't silently expire and fall back to relay.
const p2pKeepaliveInterval = 15 * time.Second

// p2pKeepaliveLoop sends periodic keepalive packets to peers with confirmed
// DirectReach to prevent NAT mappings from expiring. Without this, UDP NAT
// entries typically expire in 30-120 seconds, causing P2P paths to silently
// fail and fall back to server relay.
//
// Uses the existing hole punch packet — the peer's rate limiter (5s backoff)
// prevents amplification, so the extra traffic is minimal (~4 packets/min/peer).
func (t *Tunnel) p2pKeepaliveLoop(ctx context.Context) {
	ticker := time.NewTicker(p2pKeepaliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.sendP2PKeepalives()
		}
	}
}

// sendP2PKeepalives sends a keepalive to each peer with DirectReach=true.
func (t *Tunnel) sendP2PKeepalives() {
	t.mu.RLock()
	type peerAddr struct {
		addr *net.UDPAddr
	}
	var addrs []peerAddr
	peers := t.peerSnapshot.Load().(map[[16]byte]*Peer)
	for _, peer := range peers {
		pAddr := peer.PublicAddr.Load()
		if peer.DirectReach.Load() && pAddr != nil {
			addrs = append(addrs, peerAddr{addr: pAddr})
		}
	}
	t.mu.RUnlock()

	if len(addrs) == 0 {
		return
	}

	// Reuse cached hole punch packet — built once in handleAssignIP.
	t.mu.RLock()
	raw := t.nat.cachedPunchPacket.Load()
	t.mu.RUnlock()
	if raw == nil {
		return
	}
	packet := raw.([]byte)

	for _, pa := range addrs {
		t.sendCtrl(packet, pa.addr)
	}
}
