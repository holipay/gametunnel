package client

import (
	"context"
	"log"
	"time"

	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/protocol"
)

// stalePeerGracePeriod is how long a peer can be absent from the server's
// peer list before we consider it stale and remove it.
const stalePeerGracePeriod = 90 * time.Second

// stalePeerCheckInterval is how often we check for stale peers.
const stalePeerCheckInterval = 30 * time.Second

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
			t.sendCtrl(packet, t.serverAddr.Load())
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

	// Collect stale keys from authoritative map
	var staleKeys [][16]byte
	for key, peer := range t.peers {
		lastSeen := peer.lastSeen.Load()
		if lastSeen != 0 && now.UnixNano()-lastSeen > int64(stalePeerGracePeriod) {
			staleKeys = append(staleKeys, key)
		}
	}

	if len(staleKeys) == 0 {
		return
	}

	// Delete stale peers and update snapshot
	for _, key := range staleKeys {
		delete(t.peers, key)
	}
	t.peerSnapshot.Store(t.peers)

	for _, key := range staleKeys {
		log.Printf(i18n.T().LogCleanPeer,
			"", key, stalePeerGracePeriod)
	}
}
