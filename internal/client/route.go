package client

import (
	"net"

	"github.com/holipay/gametunnel/internal/protocol"
)

// routePacket determines how to route an outgoing IP packet.
// pkt is a slice of the TUN read buffer; it must not be retained beyond
// this call — Marshal copies the data for the UDP send.
func (t *Tunnel) routePacket(pkt []byte, srcIP, dstIP net.IP) {
	// Fast path: check server destination first (most common for relay)
	dstKey := ipKey(dstIP)
	if dstKey == t.serverIPKey {
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
		// Use p2pCipher (DirClientToClient) so both sides build the same nonce.
		data := pkt
		if t.p2pCipher != nil {
			data = t.p2pCipher.Encrypt(pkt)
		}
		dp := &protocol.DataPayload{SrcIP: srcIP, DstIP: dstIP, Data: data}
		// Pre-allocate dst buffer for single-append encoding
		dst := make([]byte, 0, protocol.HeaderLen+dp.MarshalSize()+protocol.ChecksumLen)
		encoded := protocol.AppendEncodeChecked(dst, protocol.TypeData, dp.Marshal())
		t.sendUDP(encoded, peer.PublicAddr)
	} else {
		// Fallback: relay through server.
		t.sendToServer(pkt, srcIP, dstIP)
	}
}

// sendToServer sends a packet via the server relay.
func (t *Tunnel) sendToServer(pkt []byte, srcIP, dstIP net.IP) {
	data := pkt
	if t.encCipher != nil {
		data = t.encCipher.Encrypt(pkt)
	}
	dp := &protocol.DataPayload{SrcIP: srcIP, DstIP: dstIP, Data: data}
	dst := make([]byte, 0, protocol.HeaderLen+dp.MarshalSize()+protocol.ChecksumLen)
	encoded := protocol.AppendEncodeChecked(dst, protocol.TypeData, dp.Marshal())
	t.sendUDP(encoded, t.serverAddr)
}
