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
// header(2) + srcIP(4) + dstIP(4) + flags(1) + [token(16)] + data(N) + CRC32(4).
// If token is non-nil and non-zero, DataFlagHasToken is set and token is included.
func buildDataPacket(srcIP, dstIP net.IP, data []byte, flags byte, token *[16]byte) []byte {
	tokenLen := 0
	if token != nil && *token != [16]byte{} {
		flags |= protocol.DataFlagHasToken
		tokenLen = 16
	}
	size := protocol.HeaderLen + 9 + tokenLen + len(data) + protocol.ChecksumLen
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
	if tokenLen > 0 {
		copy(dst[off:], token[:])
		off += 16
	}
	copy(dst[off:], data)
	off += len(data)
	crc := crc32.ChecksumIEEE(dst[:off])
	binary.LittleEndian.PutUint32(dst[off:], crc)
	return dst
}

// buildEncryptedDataPacket encrypts pkt and wraps it in a data packet:
// header(2) + srcIP(4) + dstIP(4) + flags(1) + encrypted.
// The flags byte is kept in cleartext so that UnmarshalDataPooled can
// correctly parse it — the encrypted blob (EncVersion + nonce + AEAD)
// follows after offset 9 and IsEncrypted reads the EncVersion prefix.
func buildEncryptedDataPacket(srcIP, dstIP net.IP, pkt []byte, cipher *crypto.Cipher, flags byte) []byte {
	encMax := crypto.Overhead + len(pkt)
	size := protocol.HeaderLen + 9 + encMax
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

	dst = dst[:off]
	dst = cipher.EncryptTo(dst, pkt)
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
	serverVersion := t.serverVersion
	var token *[16]byte
	if serverVersion >= 0x0107 {
		token = &t.sessionToken
	}
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
		t.sendToServerFEC(sendData, srcIP, dstIP, encCipher, flags, fecEnc, token)
		return
	}

	// Broadcast/multicast: relay to all peers via server
	if cachedSubnet != nil && netutil.IsRelayTarget(dstIP, cachedSubnet) {
		t.sendToServerFEC(sendData, srcIP, dstIP, encCipher, flags, fecEnc, token)
		return
	}

	if peerDirect {
		// P2P direct path — send directly for low latency.
		var packet []byte
		if p2pCipher != nil {
			packet = buildEncryptedDataPacket(srcIP, dstIP, sendData, p2pCipher, flags)
		} else {
			packet = buildDataPacket(srcIP, dstIP, sendData, flags, nil)
		}
		t.sendUDP(packet, peerAddr)
		// Generate FEC parity for P2P path
		t.feedFEC(packet, peerAddr, fecEnc)
	} else {
		// Fallback: relay through server.
		t.sendToServerFEC(sendData, srcIP, dstIP, encCipher, flags, fecEnc, token)
	}
}

// sendToServerFEC sends a packet via server relay and generates FEC parity.
func (t *Tunnel) sendToServerFEC(pkt []byte, srcIP, dstIP net.IP, cipher *crypto.Cipher, flags byte, fecEnc *netutil.FECEncoder, token *[16]byte) {
	var packet []byte
	if cipher != nil {
		packet = buildEncryptedDataPacket(srcIP, dstIP, pkt, cipher, flags)
	} else {
		packet = buildDataPacket(srcIP, dstIP, pkt, flags, token)
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
