package client

import (
	"net"

	"github.com/holipay/gametunnel/internal/protocol"
)

// routePacket determines how to route an outgoing IP packet.
func (t *Tunnel) routePacket(pkt []byte, srcIP, dstIP net.IP) {
	subnet := &net.IPNet{
		IP:   t.virtualIP.Mask(t.subnetMask),
		Mask: t.subnetMask,
	}
	if protocol.IsBroadcast(dstIP, subnet) {
		t.relayBroadcast(pkt, srcIP)
		return
	}
	if dstIP.Equal(t.serverIP) {
		t.sendToServer(pkt, srcIP, dstIP)
		return
	}

	t.mu.RLock()
	peer, ok := t.peers[dstIP.String()]
	t.mu.RUnlock()

	if ok && peer.PublicAddr != nil {
		dp := &protocol.DataPayload{SrcIP: srcIP, DstIP: dstIP, Data: pkt}
		t.sendUDP(protocol.EncodeChecked(protocol.TypeData, dp.Marshal()), peer.PublicAddr)
	} else {
		t.sendToServer(pkt, srcIP, dstIP)
	}
}

// relayBroadcast sends a broadcast packet to the server for relay.
// Only sends to server — server forwards to all peers in the room.
// Does NOT also send directly to P2P peers, as that causes duplicate
// broadcast delivery.
func (t *Tunnel) relayBroadcast(pkt []byte, srcIP net.IP) {
	dp := &protocol.DataPayload{
		SrcIP: srcIP,
		DstIP: net.IPv4(255, 255, 255, 255),
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
