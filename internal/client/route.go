package client

import (
	"encoding/binary"
	"hash/crc32"
	"net"

	"github.com/holipay/gametunnel/internal/crypto"
	"github.com/holipay/gametunnel/internal/protocol"
)

// buildDataPacket constructs a wire-format data packet: header(2) + DataPayload(8+len) + CRC32(4).
func buildDataPacket(srcIP, dstIP net.IP, data []byte) []byte {
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
	return dst
}

// buildEncryptedDataPacket encrypts pkt and wraps it in a data packet in a
// single allocation: header(2) + srcIP(4) + dstIP(4) + encrypted + CRC32(4).
// Saves 1 heap allocation per packet vs separate Encrypt + buildDataPacket.
func buildEncryptedDataPacket(srcIP, dstIP net.IP, pkt []byte, cipher *crypto.Cipher) []byte {
	// Pre-calculate final size to avoid growing the buffer.
	// encrypted size = Overhead(29) + len(pkt) + TagSize(16) but Seal appends
	// so we just allocate for header + IPs + max encrypted + CRC.
	encMax := crypto.Overhead + len(pkt) + 16 // upper bound
	size := protocol.HeaderLen + 8 + encMax + protocol.ChecksumLen
	dst := make([]byte, size)

	off := 0
	dst[off] = protocol.ProtocolVersion
	dst[off+1] = protocol.TypeData
	off += protocol.HeaderLen
	copy(dst[off:off+4], srcIP.To4())
	copy(dst[off+4:off+8], dstIP.To4())
	off += 8

	// Encrypt directly into dst — avoids separate Encrypt allocation
	dst = dst[:off] // set length to off so EncryptTo appends
	dst = cipher.EncryptTo(dst, pkt)
	off = len(dst)

	// Append CRC
	crc := crc32.ChecksumIEEE(dst[:off])
	dst = append(dst,
		byte(crc), byte(crc>>8), byte(crc>>16), byte(crc>>24),
	)
	return dst
}

// routePacket determines how to route an outgoing IP packet.
// pkt is a slice of the TUN read buffer; it must not be retained beyond
// this call — Marshal copies the data for the UDP send.
func (t *Tunnel) routePacket(pkt []byte, srcIP, dstIP net.IP) {
	// Compute dstKey once — ipKey calls To16() which allocates for IPv4.
	dstKey := ipKey(dstIP)

	// Single read lock snapshot for all fields needed in this call.
	t.mu.RLock()
	serverIPKey := t.serverIPKey
	cachedSubnet := t.cachedSubnet
	encCipher := t.encCipher
	p2pCipher := t.p2pCipher
	peer, ok := t.peers[dstKey]
	var peerAddr *net.UDPAddr
	var peerDirect bool
	if ok {
		peerAddr = peer.PublicAddr
		peerDirect = peerAddr != nil && peer.DirectReach.Load()
	}
	t.mu.RUnlock()

	// Fast path: check server destination first (most common for relay)
	if dstKey == serverIPKey {
		t.sendToServer(pkt, srcIP, dstIP, encCipher)
		return
	}

	// Broadcast/multicast: relay to all peers via server
	if cachedSubnet != nil && protocol.IsRelayTarget(dstIP, cachedSubnet) {
		t.sendToServer(pkt, srcIP, dstIP, encCipher)
		return
	}

	if peerDirect {
		// P2P direct path confirmed — send directly for low latency.
		if p2pCipher != nil {
			t.sendUDP(buildEncryptedDataPacket(srcIP, dstIP, pkt, p2pCipher), peerAddr)
		} else {
			t.sendUDP(buildDataPacket(srcIP, dstIP, pkt), peerAddr)
		}
	} else {
		// Fallback: relay through server.
		t.sendToServer(pkt, srcIP, dstIP, encCipher)
	}
}

// sendToServer sends a packet via the server relay with the given cipher.
func (t *Tunnel) sendToServer(pkt []byte, srcIP, dstIP net.IP, cipher *crypto.Cipher) {
	if cipher != nil {
		// Single allocation: encrypt + wrap in data packet
		t.sendUDP(buildEncryptedDataPacket(srcIP, dstIP, pkt, cipher), t.serverAddr)
	} else {
		t.sendUDP(buildDataPacket(srcIP, dstIP, pkt), t.serverAddr)
	}
}
