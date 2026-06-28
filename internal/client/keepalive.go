package client

import (
	"context"
	"log"
	"net"
	"time"

	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/netutil"
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

// burstHolePunch sends count hole punch packets to addr at the given interval.
// Respects context cancellation. Uses the cached hole punch packet.
func (t *Tunnel) burstHolePunch(addr *net.UDPAddr, count int, interval time.Duration, ctx context.Context) {
	t.mu.RLock()
	packet := t.cachedPunchPacket
	t.mu.RUnlock()
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
	t.sendCtrl(packet, t.serverAddr)
}

// startHolePunch initiates a multi-phase hole punch to a peer.
// It runs until all phases complete or the context is cancelled.
// Uses NAT type detection and port prediction for smarter strategies.
func (t *Tunnel) startHolePunch(ctx context.Context, peerIP net.IP) {
	t.mu.RLock()
	peer, ok := t.peers[ipKey(peerIP)]
	if !ok || peer.PublicAddr == nil {
		t.mu.RUnlock()
		return
	}
	// Snapshot PublicAddr under lock to avoid data race with handlePeerInfo
	peerAddr := peer.PublicAddr
	natResult := t.natProbeResult
	t.mu.RUnlock()

	// Determine strategy based on NAT type
	strategy := netutil.StrategyDirect
	if natResult != nil {
		// We know our NAT type; the peer's NAT type is unknown
		// Use our type to decide strategy (conservative: assume peer is restricted)
		switch natResult.Type {
		case netutil.NATSymmetric:
			// Symmetric NAT — try extended punch with port prediction
			strategy = netutil.StrategyExtended
		case netutil.NATFullCone, netutil.NATNoNAT:
			// Full Cone or no NAT — direct punch is very likely to succeed
			strategy = netutil.StrategyDirect
		}
	}

	// If Symmetric NAT detected, try port prediction first
	if strategy == netutil.StrategyExtended {
		t.mu.RLock()
		pp := t.portPredictor
		t.mu.RUnlock()
		if pp != nil {
			if predictedPorts := pp.PredictPortsForPeer([]int{peerAddr.Port}); len(predictedPorts) > 0 {
				log.Printf("[hole-punch] trying port prediction for %v: %d candidates", peerIP, len(predictedPorts))
				for _, port := range predictedPorts {
					predictedAddr := &net.UDPAddr{IP: peerAddr.IP, Port: port}
					t.burstHolePunch(predictedAddr, 3, 50*time.Millisecond, ctx)
				}
				// If prediction worked, we're done
				if t.hasDirectPeerTraffic(peerIP) {
					log.Printf(i18n.T().LogP2PSuccess, 0, peerIP)
					return
				}
			}
		}
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
	if strategy == netutil.StrategyExtended {
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
	peer, ok := t.peers[ipKey(peerIP)]
	if !ok || peer.PublicAddr == nil {
		t.mu.RUnlock()
		return
	}
	// Snapshot PublicAddr under lock to avoid data race with handlePeerInfo
	peerAddr := peer.PublicAddr
	t.mu.RUnlock()

	// Rate limit: check if we recently punched back to this peer
	if !peer.tryRateLimitHolePunch(holePunchBackoff) {
		return
	}

	// Punch back in a goroutine — don't block the receive loop.
	go func() {
		t.burstHolePunch(peerAddr, holePunchBurstPerPhase, 50*time.Millisecond, ctx)
	}()
}

// hasDirectPeerTraffic checks if we've received direct P2P traffic from a peer.
func (t *Tunnel) hasDirectPeerTraffic(peerIP net.IP) bool {
	t.mu.RLock()
	peer, ok := t.peers[ipKey(peerIP)]
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
func (t *Tunnel) keepaliveLoop(ctx context.Context, cancel context.CancelFunc) {
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
				cancel()
				return
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

	// Collect stale keys under read lock to minimize write lock hold time.
	t.mu.RLock()
	var staleKeys [][16]byte
	var stalePeers []*Peer
	for key, peer := range t.peers {
		lastSeen := peer.lastSeen.Load()
		if lastSeen != nil && now.Sub(*lastSeen) > stalePeerGracePeriod {
			staleKeys = append(staleKeys, key)
			stalePeers = append(stalePeers, peer)
		}
	}
	t.mu.RUnlock()

	if len(staleKeys) == 0 {
		return
	}

	// Delete under write lock. Re-check that the peer pointer still matches
	// to avoid deleting a freshly-added peer with the same key (handlePeerInfo
	// may have replaced the map between our read and write locks).
	t.mu.Lock()
	for i, key := range staleKeys {
		if cur, ok := t.peers[key]; ok && cur == stalePeers[i] {
			delete(t.peers, key)
		}
	}
	t.mu.Unlock()

	for _, peer := range stalePeers {
		log.Printf(i18n.T().LogCleanPeer,
			peer.Username, peer.VirtualIP, stalePeerGracePeriod)
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
		t.sendHolePunchRelay(peerIP)
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
	type peerAddr struct {
		addr *net.UDPAddr
	}
	var addrs []peerAddr
	for _, peer := range t.peers {
		if peer.DirectReach.Load() && peer.PublicAddr != nil {
			addrs = append(addrs, peerAddr{addr: peer.PublicAddr})
		}
	}
	t.mu.RUnlock()

	if len(addrs) == 0 {
		return
	}

	// Reuse cached hole punch packet — built once in handleAssignIP.
	t.mu.RLock()
	packet := t.cachedPunchPacket
	t.mu.RUnlock()

	for _, pa := range addrs {
		t.sendCtrl(packet, pa.addr)
	}
}
