package client

import (
	"encoding/binary"
	"hash/crc32"
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
		data := pkt
		if t.p2pCipher != nil {
			data = t.p2pCipher.Encrypt(pkt)
		}
		// Build encoded packet in a single buffer: header(2) + dataPayload(8+len) + checksum(4)
		// DataPayload wire format: srcIP(4) + dstIP(4) + data
		size := protocol.HeaderLen + 8 + len(data) + protocol.ChecksumLen
		dst := make([]byte, size)
		off := 0
		// Protocol header
		dst[off] = protocol.ProtocolVersion
		dst[off+1] = protocol.TypeData
		off += protocol.HeaderLen
		// DataPayload: srcIP + dstIP + data
		copy(dst[off:off+4], srcIP.To4())
		copy(dst[off+4:off+8], dstIP.To4())
		off += 8
		copy(dst[off:], data)
		off += len(data)
		// CRC32 over header+payload
		crc := crc32.ChecksumIEEE(dst[:off])
		binary.LittleEndian.PutUint32(dst[off:], crc)
		t.sendUDP(dst, peer.PublicAddr)
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
	// Build encoded packet in a single buffer to avoid DataPayload allocation
	size := protocol.HeaderLen + 8 + len(data) + protocol.ChecksumLen
	dst := make([]byte, size)
	off := 0
	dst[off] = protocol.ProtocolVersion
	dst[off+1] = protocol.TypeData
	off += protocol.HeaderLen
	copy(dst[off:off+4], srcIP.To4())
	copy(dst[off+4:off+8], dstIP.To4())
	off += 8
	copy(dst[off:], data)
	off += len(data)
	crc := crc32.ChecksumIEEE(dst[:off])
	binary.LittleEndian.PutUint32(dst[off:], crc)
	t.sendUDP(dst, t.serverAddr)
}
