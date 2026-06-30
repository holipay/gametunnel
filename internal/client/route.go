package client

import (
	"github.com/holipay/gametunnel/internal/netkey"
	"encoding/binary"
	"hash/crc32"
	"net"

	"github.com/holipay/gametunnel/internal/crypto"
	"github.com/holipay/gametunnel/internal/netutil"
	"github.com/holipay/gametunnel/internal/protocol"
)

// buildDataPacket constructs a wire-format data packet:
// header(2) + srcIP(4) + dstIP(4) + flags(1) + [token(16)] + data(N) + CRC32(4).
// If token is non-zero, DataFlagHasToken is set and token is included.
func buildDataPacket(srcIP, dstIP net.IP, data []byte, flags byte, token [16]byte) []byte {
	tokenLen := 0
	if token != [16]byte{} {
		flags |= protocol.DataFlagHasToken
		tokenLen = 16
	}
	payloadLen := protocol.DataHeaderLen + tokenLen + len(data)
	size := protocol.HeaderLen + payloadLen + protocol.ChecksumLen
	dst := make([]byte, size)
	off := 0
	dst[off] = protocol.ProtocolVersion
	dst[off+1] = protocol.TypeData
	off += protocol.HeaderLen
	copy(dst[off:off+4], srcIP.To4())
	copy(dst[off+4:off+8], dstIP.To4())
	off += 8
	dst[off] = protocol.DataFormatVersion
	off++
	dst[off] = flags
	off++
	if tokenLen > 0 {
		copy(dst[off:], token[:])
		off += protocol.DataTokenLen
	}
	copy(dst[off:], data)
	off += len(data)
	crc := crc32.ChecksumIEEE(dst[:off])
	binary.LittleEndian.PutUint32(dst[off:], crc)
	return dst
}

// buildEncryptedDataPacket encrypts pkt and wraps it in a data packet:
// header(2) + srcIP(4) + dstIP(4) + flags(1) + [token(16)] + encrypted.
// The flags byte and token are kept in cleartext so that UnmarshalDataPooled
// can correctly parse them — the encrypted blob (EncVersion + nonce + AEAD)
// follows after the token and IsEncrypted reads the EncVersion prefix.
func buildEncryptedDataPacket(srcIP, dstIP net.IP, pkt []byte, cipher *crypto.Cipher, flags byte, token [16]byte) []byte {
	tokenLen := 0
	if token != [16]byte{} {
		flags |= protocol.DataFlagHasToken
		tokenLen = 16
	}
	encMax := crypto.Overhead + len(pkt)
	payloadLen := protocol.DataHeaderLen + tokenLen
	size := protocol.HeaderLen + payloadLen + encMax
	dst := make([]byte, size)

	off := 0
	dst[off] = protocol.ProtocolVersion
	dst[off+1] = protocol.TypeData
	off += protocol.HeaderLen
	copy(dst[off:off+4], srcIP.To4())
	copy(dst[off+4:off+8], dstIP.To4())
	off += 8
	dst[off] = protocol.DataFormatVersion
	off++
	dst[off] = flags
	off++
	if tokenLen > 0 {
		copy(dst[off:], token[:])
		off += protocol.DataTokenLen
	}

	dst = dst[:off]
	dst = cipher.EncryptTo(dst, pkt)
	return dst
}

// routePacket determines how to route an outgoing IP packet.
// pkt is a slice of the TUN read buffer; it must not be retained beyond
// this call — Marshal copies the data for the UDP send.
// srcIP/dstIP use [4]byte to avoid net.IP heap escape in the caller; they
// are converted to net.IP here where the result stays on the worker stack.
func (t *Tunnel) routePacket(pkt []byte, srcIP, dstIP [4]byte) {
	dstKey := netkey.IPKey(dstIP[:])

	// Single read lock snapshot for all fields needed in this call.
	t.mu.RLock()
	var serverIPKey [16]byte
	if p := t.session.serverIPKey.Load(); p != nil {
		serverIPKey = *p
	}
	serverAddr := t.serverAddr.Load()
	cachedSubnet := t.session.cachedSubnet.Load()
	p2pCipher := t.crypto.p2pCipher
	serverVersion := t.session.serverVersion.Load()
	var token [16]byte
	if serverVersion >= uint32(protocol.MinTokenVersion) {
		token = t.session.sessionToken
	}
	peer, ok := t.peers[dstKey]
	var peerAddr *net.UDPAddr
	var peerDirect bool
	if ok {
		peerAddr = peer.PublicAddr.Load()
		peerDirect = peerAddr != nil && peer.DirectReach.Load()
	}
	t.mu.RUnlock()

	srcNet := net.IP(srcIP[:])
	dstNet := net.IP(dstIP[:])

	// Fast path: check server destination first (most common for relay)
	if dstKey == serverIPKey {
		t.sendToServer(pkt, srcNet, dstNet, p2pCipher, token, serverAddr)
		return
	}

	// Broadcast/multicast: relay to all peers via server
	if cachedSubnet != nil && netutil.IsRelayTarget(dstNet, cachedSubnet) {
		t.sendToServer(pkt, srcNet, dstNet, p2pCipher, token, serverAddr)
		return
	}

	if peerDirect {
		// P2P direct path — send directly for low latency.
		var packet []byte
		if p2pCipher != nil {
			packet = buildEncryptedDataPacket(srcNet, dstNet, pkt, p2pCipher, 0, token)
		} else {
			packet = buildDataPacket(srcNet, dstNet, pkt, 0, token)
		}
		t.sendUDP(packet, peerAddr)
	} else {
		// Fallback: relay through server.
		t.sendToServer(pkt, srcNet, dstNet, p2pCipher, token, serverAddr)
	}
}

// sendToServer sends a packet via server relay.
// serverAddr is passed explicitly (snapshotted under lock) to avoid data races
// with Connect() reassigning t.serverAddr.
func (t *Tunnel) sendToServer(data []byte, srcIP, dstIP net.IP, cipher *crypto.Cipher, token [16]byte, serverAddr *net.UDPAddr) {
	var packet []byte
	if cipher != nil {
		packet = buildEncryptedDataPacket(srcIP, dstIP, data, cipher, 0, token)
	} else {
		packet = buildDataPacket(srcIP, dstIP, data, 0, token)
	}
	t.sendUDP(packet, serverAddr)
}
