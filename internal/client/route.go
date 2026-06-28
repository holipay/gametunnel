package client

import (
	"encoding/binary"
	"hash/crc32"
	"net"

	"github.com/holipay/gametunnel/internal/crypto"
	"github.com/holipay/gametunnel/internal/netutil"
	"github.com/holipay/gametunnel/internal/protocol"
)

// buildDataPacket constructs a wire-format data packet:
// header(2) + DataPayload(9+len) + CRC32(4).
func buildDataPacket(srcIP, dstIP net.IP, data []byte, flags byte) []byte {
	size := protocol.HeaderLen + 9 + len(data) + protocol.ChecksumLen
	dst := make([]byte, size)
	off := 0
	dst[off] = protocol.ProtocolVersion
	dst[off+1] = protocol.TypeData
	off += protocol.HeaderLen
	copy(dst[off:off+4], srcIP.To4())
	copy(dst[off+4:off+8], dstIP.To4())
	off += 8
	dst[off] = flags
	off++
	copy(dst[off:], data)
	off += len(data)
	crc := crc32.ChecksumIEEE(dst[:off])
	binary.LittleEndian.PutUint32(dst[off:], crc)
	return dst
}

// buildEncryptedDataPacket encrypts pkt and wraps it in a data packet:
// header(2) + srcIP(4) + dstIP(4) + flags(1) + encrypted.
func buildEncryptedDataPacket(srcIP, dstIP net.IP, pkt []byte, cipher *crypto.Cipher, flags byte) []byte {
	// Include flags byte in the plaintext to be encrypted
	plaintext := make([]byte, 1+len(pkt))
	plaintext[0] = flags
	copy(plaintext[1:], pkt)

	encMax := crypto.Overhead + len(plaintext)
	size := protocol.HeaderLen + 8 + encMax
	dst := make([]byte, size)

	off := 0
	dst[off] = protocol.ProtocolVersion
	dst[off+1] = protocol.TypeData
	off += protocol.HeaderLen
	copy(dst[off:off+4], srcIP.To4())
	copy(dst[off+4:off+8], dstIP.To4())
	off += 8

	dst = dst[:off]
	dst = cipher.EncryptTo(dst, plaintext)
	return dst[:len(dst)]
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
	lz4Enc := t.lz4Encoder
	fecEnc := t.fecEncoder
	t.mu.RUnlock()

	// Try LZ4 compression
	var flags byte
	sendData := pkt
	if lz4Enc != nil {
		if compressed := lz4Enc.Compress(pkt); compressed != nil {
			sendData = compressed
			flags = protocol.DataFlagCompressed
		}
	}

	// Fast path: check server destination first (most common for relay)
	if dstKey == serverIPKey {
		t.sendToServerFEC(sendData, srcIP, dstIP, encCipher, flags, fecEnc)
		return
	}

	// Broadcast/multicast: relay to all peers via server
	if cachedSubnet != nil && netutil.IsRelayTarget(dstIP, cachedSubnet) {
		t.sendToServerFEC(sendData, srcIP, dstIP, encCipher, flags, fecEnc)
		return
	}

	if peerDirect {
		// P2P direct path — send directly for low latency.
		var packet []byte
		if p2pCipher != nil {
			packet = buildEncryptedDataPacket(srcIP, dstIP, sendData, p2pCipher, flags)
		} else {
			packet = buildDataPacket(srcIP, dstIP, sendData, flags)
		}
		t.sendUDP(packet, peerAddr)
		// Generate FEC parity for P2P path
		t.feedFEC(packet, peerAddr, fecEnc)
	} else {
		// Fallback: relay through server.
		t.sendToServerFEC(sendData, srcIP, dstIP, encCipher, flags, fecEnc)
	}
}

// sendToServerFEC sends a packet via server relay and generates FEC parity.
func (t *Tunnel) sendToServerFEC(pkt []byte, srcIP, dstIP net.IP, cipher *crypto.Cipher, flags byte, fecEnc *netutil.FECEncoder) {
	var packet []byte
	if cipher != nil {
		packet = buildEncryptedDataPacket(srcIP, dstIP, pkt, cipher, flags)
	} else {
		packet = buildDataPacket(srcIP, dstIP, pkt, flags)
	}
	t.sendUDP(packet, t.serverAddr)
	t.feedFEC(packet, t.serverAddr, fecEnc)
}

// feedFEC feeds a sent packet to the FEC encoder and sends any generated
// parity packet to the same destination.
func (t *Tunnel) feedFEC(packet []byte, dest *net.UDPAddr, fecEnc *netutil.FECEncoder) {
	if fecEnc == nil {
		return
	}
	// Feed the packet data (after protocol header) to the FEC encoder
	parity := fecEnc.Encode(packet[protocol.HeaderLen:])
	if parity != nil {
		t.sendUDP(parity, dest)
	}
}
