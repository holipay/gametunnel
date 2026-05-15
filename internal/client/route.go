package client

import (
	"net"

	"github.com/holipay/gametunnel-protocol/protocol"
)

// routePacket determines how to route an outgoing IP packet.
// pkt is a slice of the TUN read buffer; it must not be retained beyond
// this call — Marshal copies the data for the UDP send.
func (t *Tunnel) routePacket(pkt []byte, srcIP, dstIP net.IP) {
	// Fast path: check server destination first (most common for relay)
	dstKey := ip4Key(dstIP)
	if dstKey == t.serverIP4 {
		t.sendToServer(pkt, srcIP, dstIP)
		return
	}

	// Broadcast/multicast: relay to all peers via server
	if t.cachedSubnet != nil && protocol.IsRelayTarget(dstIP, t.cachedSubnet) {
		t.sendToServer(pkt, srcIP, dstIP)
		return
	}

	// Peer lookup
	t.mu.RLock()
	peer, ok := t.peers[dstKey]
	t.mu.RUnlock()

	if ok && peer.PublicAddr != nil && peer.DirectReach.Load() {
		// P2P direct path confirmed — send directly for low latency.
		dp := &protocol.DataPayload{SrcIP: srcIP, DstIP: dstIP, Data: pkt}
		t.sendUDP(protocol.EncodeChecked(protocol.TypeData, dp.Marshal()), peer.PublicAddr)
	} else {
		// Fallback: relay through server.
		t.sendToServer(pkt, srcIP, dstIP)
	}
}

// sendToServer sends a packet via the server relay.
func (t *Tunnel) sendToServer(pkt []byte, srcIP, dstIP net.IP) {
	dp := &protocol.DataPayload{SrcIP: srcIP, DstIP: dstIP, Data: pkt}
	encoded := protocol.EncodeChecked(protocol.TypeData, dp.Marshal())
	t.sendUDP(encoded, t.serverAddr)
}
