package client

import (
	"context"
	"log"
	"net"
	"time"

	"github.com/holipay/gametunnel/internal/i18n"
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

// stalePeerGracePeriod is how long a peer can be absent from the server's
// peer list before we consider it stale and remove it.
const stalePeerGracePeriod = 90 * time.Second

// stalePeerCheckInterval is how often we check for stale peers.
const stalePeerCheckInterval = 30 * time.Second

// holePunchBackoff limits how often we respond to hole punch requests
// from the same peer. Prevents amplification attacks.
const holePunchBackoff = 5 * time.Second

// p2pKeepaliveInterval is how often we send a keepalive packet to peers
// with confirmed DirectReach. Keeps NAT mappings alive so the P2P path
// doesn't silently expire and fall back to relay.
const p2pKeepaliveInterval = 15 * time.Second

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
			t.sendCtrl(packet, peer.PublicAddr)
			time.Sleep(interval)
		}

		// If peer is already reachable via P2P after this phase, stop early.
		// This is checked by seeing if we've received any direct data from them.
		if t.hasDirectPeerTraffic(peerIP) {
			log.Printf(i18n.T().LogP2PSuccess, phase+1, peerIP)
			return
		}
	}

	log.Printf(i18n.T().LogP2PFailed, peerIP)
}

// handleHolePunchReceived processes an incoming hole punch request (bidirectional).
// When A punches B, B receives A's public address from the server and immediately
// punches back. This creates NAT mappings on BOTH sides simultaneously.
//
// Rate-limited: responds at most once per peer per holePunchBackoff interval
// to prevent amplification attacks.
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

	// Rate limit: check if we recently punched back to this peer
	now := time.Now()
	lastPunch := peer.lastPunchBack.Load()
	if lastPunch != nil && now.Sub(*lastPunch) < holePunchBackoff {
		return
	}
	peer.lastPunchBack.Store(&now)

	// Punch back in a goroutine — don't block the receive loop.
	go func() {
		punchPayload := make([]byte, 4)
		copy(punchPayload, t.virtualIP.To4())
		packet := protocol.EncodeChecked(protocol.TypeHolePunch, punchPayload)

		for i := 0; i < holePunchBurstPerPhase; i++ {
			t.sendCtrl(packet, peer.PublicAddr)
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

// keepaliveLoop sends periodic keepalive packets to the server and
// tracks the last time we received any data from the server (pong, peer info, etc.).
// If the server appears dead for too long, the tunnel context is cancelled
// to trigger a reconnect.
func (t *Tunnel) keepaliveLoop(ctx context.Context) {
	const serverTimeout = 30 * time.Second // 3 missed keepalives

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	packet := protocol.EncodeChecked(protocol.TypeKeepAlive, nil)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.sendCtrl(packet, t.serverAddr)

			// Check if server is still alive
			lastSeen := t.lastServerResponse.Load()
			if lastSeen != nil && time.Since(*lastSeen) > serverTimeout {
				log.Printf(i18n.T().LogServerTimeout, serverTimeout)
				// Don't cancel ctx here — let the outer connectLoop handle reconnection.
				// The ReadFromUDP goroutine will eventually fail and exit.
			}
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
			t.sendCtrl(packet, t.serverAddr)
		}
	}
}

// stalePeerCleanupLoop removes peers that haven't been seen in the server's
// peer list for too long. This handles cases where a peer disconnects
// ungracefully (crash, network drop) without sending a proper leave.
func (t *Tunnel) stalePeerCleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(stalePeerCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.cleanStalePeers()
		}
	}
}

// cleanStalePeers removes peers whose lastSeen timestamp is too old.
func (t *Tunnel) cleanStalePeers() {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()

	for key, peer := range t.peers {
		lastSeen := peer.lastSeen.Load()
		if lastSeen != nil && now.Sub(*lastSeen) > stalePeerGracePeriod {
			log.Printf(i18n.T().LogCleanPeer,
				peer.Username, peer.VirtualIP, stalePeerGracePeriod)
			delete(t.peers, key)
		}
	}
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
		if peer.PublicAddr != nil && !peer.DirectReach.Load() {
			retryPeers = append(retryPeers, peer.VirtualIP)
		}
	}
	t.mu.RUnlock()

	if len(retryPeers) == 0 {
		return
	}

	log.Printf(i18n.T().LogRetryPunch, len(retryPeers))
	for _, peerIP := range retryPeers {
		go t.startHolePunch(ctx, peerIP)
	}
}

// markServerResponse records that we received data from the server.
// Called from handleServerData to keep the liveness tracker updated.
func (t *Tunnel) markServerResponse() {
	now := time.Now()
	t.lastServerResponse.Store(&now)
}

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
	var directPeers []*Peer
	for _, peer := range t.peers {
		if peer.DirectReach.Load() && peer.PublicAddr != nil {
			directPeers = append(directPeers, peer)
		}
	}
	t.mu.RUnlock()

	if len(directPeers) == 0 {
		return
	}

	// Reuse hole punch packet — lightweight, peer already handles it.
	payload := make([]byte, 4)
	copy(payload, t.virtualIP.To4())
	packet := protocol.EncodeChecked(protocol.TypeHolePunch, payload)

	for _, peer := range directPeers {
		t.sendCtrl(packet, peer.PublicAddr)
	}
}
