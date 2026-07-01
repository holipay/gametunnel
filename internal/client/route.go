package client

import (
	"encoding/binary"
	"hash/crc32"
	"net"

	"github.com/holipay/gametunnel/internal/crypto"
	"github.com/holipay/gametunnel/internal/netkey"
	"github.com/holipay/gametunnel/internal/netutil"
	"github.com/holipay/gametunnel/internal/protocol"
)

// buildDataHeader writes the common data packet header into dst and returns the offset after it.
// header(2) + srcIP(4) + dstIP(4) + flags(1) + [token(16)]
func buildDataHeader(dst []byte, srcIP, dstIP net.IP, flags byte, token [16]byte) int {
	if token != [16]byte{} {
		flags |= protocol.DataFlagHasToken
	}
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
	if flags&protocol.DataFlagHasToken != 0 {
		copy(dst[off:], token[:])
		off += protocol.DataTokenLen
	}
	return off
}

// buildDataPacket constructs a wire-format data packet:
// header(2) + srcIP(4) + dstIP(4) + flags(1) + [token(16)] + data(N) + CRC32(4).
func buildDataPacket(srcIP, dstIP net.IP, data []byte, flags byte, token [16]byte) []byte {
	tokenLen := 0
	if token != [16]byte{} {
		flags |= protocol.DataFlagHasToken
		tokenLen = 16
	}
	size := protocol.HeaderLen + protocol.DataHeaderLen + tokenLen + len(data) + protocol.ChecksumLen
	dst := make([]byte, size)
	off := buildDataHeader(dst, srcIP, dstIP, flags, token)
	copy(dst[off:], data)
	off += len(data)
	crc := crc32.ChecksumIEEE(dst[:off])
	binary.LittleEndian.PutUint32(dst[off:], crc)
	return dst
}

// buildEncryptedDataPacket encrypts pkt and wraps it in a data packet.
func buildEncryptedDataPacket(srcIP, dstIP net.IP, pkt []byte, cipher *crypto.Cipher, flags byte, token [16]byte) []byte {
	tokenLen := 0
	if token != [16]byte{} {
		flags |= protocol.DataFlagHasToken
		tokenLen = 16
	}
	size := protocol.HeaderLen + protocol.DataHeaderLen + tokenLen + crypto.Overhead + len(pkt)
	dst := make([]byte, size)
	off := buildDataHeader(dst, srcIP, dstIP, flags, token)
	dst = dst[:off]
	dst = cipher.EncryptTo(dst, pkt)
	return dst
}

// buildPacket builds an encrypted or plaintext data packet depending on cipher.
func buildPacket(srcIP, dstIP net.IP, data []byte, cipher *crypto.Cipher, flags byte, token [16]byte) []byte {
	if cipher != nil {
		return buildEncryptedDataPacket(srcIP, dstIP, data, cipher, flags, token)
	}
	return buildDataPacket(srcIP, dstIP, data, flags, token)
}

// routePacket determines how to route an outgoing IP packet.
func (t *Tunnel) routePacket(pkt []byte, srcIP, dstIP [4]byte) {
	dstKey := netkey.IPKey(dstIP[:])

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

	if dstKey == serverIPKey || (cachedSubnet != nil && netutil.IsRelayTarget(dstNet, cachedSubnet)) {
		t.sendToServer(pkt, srcNet, dstNet, p2pCipher, token, serverAddr)
		return
	}

	if peerDirect {
		t.sendUDP(buildPacket(srcNet, dstNet, pkt, p2pCipher, 0, token), peerAddr)
	} else {
		t.sendToServer(pkt, srcNet, dstNet, p2pCipher, token, serverAddr)
	}
}

// sendToServer sends a packet via server relay.
func (t *Tunnel) sendToServer(data []byte, srcIP, dstIP net.IP, cipher *crypto.Cipher, token [16]byte, serverAddr *net.UDPAddr) {
	t.sendUDP(buildPacket(srcIP, dstIP, data, cipher, 0, token), serverAddr)
}
