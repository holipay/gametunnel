package client

import (
	"context"
	"log"
	"net"
	"time"

	"github.com/holipay/gametunnel-protocol/protocol"
)

// holePunchIntervals defines progressive send intervals for hole punching.
// Phase 1: rapid-fire (100ms) to quickly punch through cone NATs.
// Phase 2: moderate (250ms) for port-restricted NATs that need more time.
// Phase 3: slow (500ms) for symmetric NATs or flaky networks.
var holePunchIntervals = []time.Duration{
	100 * time.Millisecond,
	250 * time.Millisecond,
	500 * time.Millisecond,
}

const holePunchBurstPerPhase = 5

// startHolePunch initiates a multi-phase hole punch to a peer.
// It runs until all phases complete or the context is cancelled.
func (t *Tunnel) startHolePunch(ctx context.Context, peerIP net.IP) {
	t.mu.RLock()
	peer, ok := t.peers[ip4Key(peerIP)]
	t.mu.RUnlock()
	if !ok || peer.PublicAddr == nil {
		return
	}

	// Build punch packet with OUR virtualIP so the peer knows who we are
	punchPayload := make([]byte, 4)
	copy(punchPayload, t.virtualIP.To4())
	packet := protocol.EncodeChecked(protocol.TypeHolePunch, punchPayload)

	for phase, interval := range holePunchIntervals {
		for i := 0; i < holePunchBurstPerPhase; i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			t.sendUDP(packet, peer.PublicAddr)
			time.Sleep(interval)
		}

		// If peer is already reachable via P2P after this phase, stop early.
		// This is checked by seeing if we've received any direct data from them.
		if t.hasDirectPeerTraffic(peerIP) {
			log.Printf("[tunnel] P2P 打洞成功 (phase %d): %s", phase+1, peerIP)
			return
		}
	}

	log.Printf("[tunnel] P2P 打洞完成（未确认直通），将通过中继通信: %s", peerIP)
}

// handleHolePunchReceived processes an incoming hole punch request (bidirectional).
// When A punches B, B receives A's public address from the server and immediately
// punches back. This creates NAT mappings on BOTH sides simultaneously.
func (t *Tunnel) handleHolePunchReceived(payload []byte) {
	if len(payload) < 4 {
		return
	}
	peerIP := net.IP(append([]byte(nil), payload[:4]...))

	t.mu.RLock()
	peer, ok := t.peers[ip4Key(peerIP)]
	t.mu.RUnlock()
	if !ok || peer.PublicAddr == nil {
		return
	}

	// Punch back in a goroutine — don't block the receive loop.
	// The 5×50ms burst takes 250ms, during which we'd drop incoming packets.
	go func() {
		punchPayload := make([]byte, 4)
		copy(punchPayload, t.virtualIP.To4())
		packet := protocol.EncodeChecked(protocol.TypeHolePunch, punchPayload)

		for i := 0; i < holePunchBurstPerPhase; i++ {
			t.sendUDP(packet, peer.PublicAddr)
			time.Sleep(50 * time.Millisecond)
		}
	}()
}

// hasDirectPeerTraffic checks if we've received direct P2P traffic from a peer.
func (t *Tunnel) hasDirectPeerTraffic(peerIP net.IP) bool {
	t.mu.RLock()
	peer, ok := t.peers[ip4Key(peerIP)]
	t.mu.RUnlock()
	if !ok {
		return false
	}
	return peer.DirectReach.Load()
}

// keepaliveLoop sends periodic keepalive packets to the server.
func (t *Tunnel) keepaliveLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	packet := protocol.EncodeChecked(protocol.TypeKeepAlive, nil)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.sendUDP(packet, t.serverAddr)
		}
	}
}

// peerDiscoveryLoop periodically requests the peer list from the server.
func (t *Tunnel) peerDiscoveryLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	packet := protocol.EncodeChecked(protocol.TypePeerRequest, nil)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.sendUDP(packet, t.serverAddr)
		}
	}
}
