package client

import (
	"context"
	"net"
	"time"

	"github.com/holipay/gametunnel/internal/protocol"
)

// startHolePunch sends NAT hole punch packets to a peer.
func (t *Tunnel) startHolePunch(peerIP net.IP) {
	t.mu.RLock()
	peer, ok := t.peers[peerIP.String()]
	t.mu.RUnlock()
	if !ok || peer.PublicAddr == nil {
		return
	}

	punchPayload := make([]byte, 4)
	copy(punchPayload, peerIP.To4())
	packet := protocol.EncodeChecked(protocol.TypeHolePunch, punchPayload)

	for i := 0; i < 5; i++ {
		t.sendUDP(packet, peer.PublicAddr)
		time.Sleep(200 * time.Millisecond)
	}
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
