package client

import (
	"net"

	"github.com/holipay/gametunnel-protocol/protocol"
)

// routePacket determines how to route an outgoing IP packet.
func (t *Tunnel) routePacket(pkt []byte, srcIP, dstIP net.IP) {
	// Broadcast (255.255.255.255, subnet broadcast) and multicast (224.0.0.0/4)
	// are relayed to all peers via the server.
	if t.cachedSubnet != nil && protocol.IsRelayTarget(dstIP, t.cachedSubnet) {
		t.relayBroadcast(pkt, srcIP, dstIP)
		return
	}
	if ip4Key(dstIP) == t.serverIP4 {
		t.sendToServer(pkt, srcIP, dstIP)
		return
	}

	t.mu.RLock()
	peer, ok := t.peers[ip4Key(dstIP)]
	t.mu.RUnlock()

	if ok && peer.PublicAddr != nil && peer.DirectReach.Load() {
		// P2P direct path confirmed — send directly for low latency.
		dp := &protocol.DataPayload{SrcIP: srcIP, DstIP: dstIP, Data: pkt}
		t.sendUDP(protocol.EncodeChecked(protocol.TypeData, dp.Marshal()), peer.PublicAddr)
	} else {
		// Fallback: peer unknown, no public address, or P2P not confirmed.
		// Relay through server. This covers:
		//   - Hole punch in progress (PublicAddr set but DirectReach false)
		//   - Hole punch failed (PublicAddr nil)
		//   - Peer behind symmetric NAT (always relay)
		t.sendToServer(pkt, srcIP, dstIP)
	}
}

// relayBroadcast sends a broadcast/multicast packet to the server for relay.
// Server forwards to all peers in the room.
// Preserves the original dstIP (255.255.255.255, subnet broadcast, or multicast).
func (t *Tunnel) relayBroadcast(pkt []byte, srcIP, dstIP net.IP) {
	dp := &protocol.DataPayload{
		SrcIP: srcIP,
		DstIP: dstIP, // preserve original broadcast/multicast destination
		Data:  pkt,
	}
	encoded := protocol.EncodeChecked(protocol.TypeData, dp.Marshal())
	t.sendUDP(encoded, t.serverAddr)
}

// sendToServer sends a unicast packet via the server relay.
func (t *Tunnel) sendToServer(pkt []byte, srcIP, dstIP net.IP) {
	dp := &protocol.DataPayload{SrcIP: srcIP, DstIP: dstIP, Data: pkt}
	t.sendUDP(protocol.EncodeChecked(protocol.TypeData, dp.Marshal()), t.serverAddr)
}
