package client

import (
	"github.com/holipay/gametunnel/internal/netkey"
	"context"
	"log"
	"net"
	"time"

	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/nat"
	"github.com/holipay/gametunnel/internal/protocol"
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

// holePunchRetryInterval is how often we retry hole punching for peers
// that haven't achieved DirectReach. NAT mappings can expire or change,
// so periodic retries improve reliability.
const holePunchRetryInterval = 25 * time.Second

// holePunchBackoff limits how often we respond to hole punch requests
// from the same peer. Prevents amplification attacks.
const holePunchBackoff = 5 * time.Second

// burstHolePunch sends count hole punch packets to addr at the given interval.
// Respects context cancellation. Uses the cached hole punch packet.
func (t *Tunnel) burstHolePunch(addr *net.UDPAddr, count int, interval time.Duration, ctx context.Context) {
	t.mu.RLock()
	raw := t.nat.cachedPunchPacket.Load()
	t.mu.RUnlock()
	if raw == nil {
		return
	}
	packet := raw.([]byte)
	for i := 0; i < count; i++ {
		select {
		case <-ctx.Done():
			return
		default:
		}
		t.sendCtrl(packet, addr)
		time.Sleep(interval)
	}
}

// sendHolePunchRelay sends a TypeHolePunch to the server to request
// server-relayed hole punch signaling. The server forwards the signal
// to the destination peer, who then punches back directly. This is
// essential for IPv6 peers where firewalls may block initial direct
// packets but allow responses after the peer initiates its own flow.
func (t *Tunnel) sendHolePunchRelay(peerIP net.IP) {
	if peerIP == nil || len(peerIP.To4()) != 4 {
		return
	}
	packet := protocol.EncodeChecked(protocol.TypeHolePunch, peerIP.To4())
	t.sendCtrl(packet, t.serverAddr.Load())
}

// startHolePunch initiates a multi-phase hole punch to a peer.
// It runs until all phases complete or the context is cancelled.
// Uses NAT type detection and port prediction for smarter strategies.
func (t *Tunnel) startHolePunch(ctx context.Context, peerIP net.IP) {
	// Wait for async NAT probe to complete before deciding strategy
	t.mu.RLock()
	probeDone := t.nat.probeDone
	t.mu.RUnlock()
	if probeDone != nil {
		select {
		case <-probeDone:
		case <-ctx.Done():
			return
		}
	}

	t.mu.RLock()
	peer, ok := t.peers[netkey.IPKey(peerIP)]
	if !ok || peer.PublicAddr.Load() == nil {
		t.mu.RUnlock()
		return
	}
	peerAddr := peer.PublicAddr.Load()
	peerNATType := peer.NATType
	natResult := t.nat.probeResult
	t.mu.RUnlock()

	// Determine strategy based on both local AND peer NAT types
	strategy := nat.StrategyDirect
	if natResult != nil && peerNATType != 0 {
		// Both sides know their NAT types — use optimal strategy
		strategy = nat.GetHolePunchStrategy(natResult.Type, peerNATType)
	} else if natResult != nil {
		// Only local NAT type known — conservative: assume peer is restricted
		switch natResult.Type {
		case nat.NATSymmetric:
			strategy = nat.StrategyExtended
		case nat.NATFullCone, nat.NATNoNAT:
			strategy = nat.StrategyDirect
		}
	}

	// Skip hole punch entirely if strategy says relay is the only option
	if strategy == nat.StrategyRelay {
		log.Printf("[hole-punch] skipping P2P for %v (both sides symmetric NAT, relay only)", peerIP)
		return
	}

	// Standard hole punch phases
	for phase, interval := range holePunchIntervals {
		t.burstHolePunch(peerAddr, holePunchBurstPerPhase, interval, ctx)
		if ctx.Err() != nil {
			return
		}

		// If peer is already reachable via P2P after this phase, stop early.
		if t.hasDirectPeerTraffic(peerIP) {
			log.Printf(i18n.T().LogP2PSuccess, phase+1, peerIP)
			return
		}
	}

	// If extended strategy failed, log with extra info
	if strategy == nat.StrategyExtended {
		log.Printf("[hole-punch] P2P failed for %v (symmetric NAT, port prediction insufficient)", peerIP)
	} else {
		log.Printf(i18n.T().LogP2PFailed, peerIP)
	}
}

// handleHolePunchReceived processes an incoming hole punch request (bidirectional).
// When A punches B, B receives A's public address from the server and immediately
// punches back. This creates NAT mappings on BOTH sides simultaneously.
//
// Rate-limited: responds at most once per peer per holePunchBackoff interval
// to prevent amplification attacks.
func (t *Tunnel) handleHolePunchReceived(ctx context.Context, payload []byte) {
	if len(payload) < 4 {
		return
	}
	peerIP := net.IP(append([]byte(nil), payload[:4]...))

	t.mu.RLock()
	peer, ok := t.peers[netkey.IPKey(peerIP)]
	if !ok || peer.PublicAddr.Load() == nil {
		t.mu.RUnlock()
		return
	}
	peerAddr := peer.PublicAddr.Load()
	t.mu.RUnlock()

	// Rate limit: check if we recently punched back to this peer
	if !peer.tryRateLimitHolePunch(holePunchBackoff) {
		return
	}

	// Punch back in a goroutine — don't block the receive loop.
	t.holePunchWg.Add(1)
	go func() {
		defer t.holePunchWg.Done()
		t.burstHolePunch(peerAddr, holePunchBurstPerPhase, 50*time.Millisecond, ctx)
	}()
}

// hasDirectPeerTraffic checks if we've received direct P2P traffic from a peer.
func (t *Tunnel) hasDirectPeerTraffic(peerIP net.IP) bool {
	t.mu.RLock()
	peer, ok := t.peers[netkey.IPKey(peerIP)]
	t.mu.RUnlock()
	if !ok {
		return false
	}
	return peer.DirectReach.Load()
}

// holePunchRetryLoop periodically retries hole punching for peers that
// haven't achieved DirectReach. NAT mappings can expire, and peers behind
// symmetric NATs may need periodic re-punching.
func (t *Tunnel) holePunchRetryLoop(ctx context.Context) {
	ticker := time.NewTicker(holePunchRetryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.retryFailedHolePunches(ctx)
		}
	}
}

// retryFailedHolePunches re-initiates hole punching for all peers that have
// a public address but haven't confirmed DirectReach.
func (t *Tunnel) retryFailedHolePunches(ctx context.Context) {
	t.mu.RLock()
	var retryPeers []net.IP
	for _, peer := range t.peers {
		if peer.PublicAddr.Load() != nil && !peer.DirectReach.Load() {
			retryPeers = append(retryPeers, peer.VirtualIP)
		}
	}
	t.mu.RUnlock()

	if len(retryPeers) == 0 {
		return
	}

	log.Printf(i18n.T().LogRetryPunch, len(retryPeers))
	for _, peerIP := range retryPeers {
		t.holePunchWg.Add(1)
		go func(ip net.IP) {
			defer t.holePunchWg.Done()
			t.startHolePunch(ctx, ip)
		}(peerIP)
		t.sendHolePunchRelay(peerIP)
	}
}
