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
// If token is non-zero, DataFlagHasToken is set and token is included.
func buildDataPacket(srcIP, dstIP net.IP, data []byte, flags byte, token [16]byte) []byte {
	tokenLen := 0
	if token != [16]byte{} {
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
	size := protocol.HeaderLen + 9 + tokenLen + encMax
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

	dst = dst[:off]
	dst = cipher.EncryptTo(dst, pkt)
	return dst[:len(dst)]
}

// routePacket determines how to route an outgoing IP packet.
// pkt is a slice of the TUN read buffer; it must not be retained beyond
// this call — Marshal copies the data for the UDP send.
// srcIP/dstIP use [4]byte to avoid net.IP heap escape in the caller; they
// are converted to net.IP here where the result stays on the worker stack.
func (t *Tunnel) routePacket(pkt []byte, srcIP, dstIP [4]byte) {
	dstKey := ipKey(dstIP[:])

	// Single read lock snapshot for all fields needed in this call.
	t.mu.RLock()
	serverIPKey, _ := t.serverIPKey.Load().([16]byte)
	serverAddr := t.serverAddr.Load()
	cachedSubnet := t.cachedSubnet.Load()
	encCipher := t.encCipher
	p2pCipher := t.p2pCipher
	serverVersion := t.serverVersion.Load()
	var token [16]byte
	if serverVersion >= uint32(protocol.MinTokenVersion) {
		token = t.sessionToken
	}
	peer, ok := t.peers[dstKey]
	var peerAddr *net.UDPAddr
	var peerDirect bool
	if ok {
		peerAddr = peer.PublicAddr.Load()
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

	// Embed FEC header (groupID + seq) at end of data when FEC is enabled.
	// The encoder receives only the raw IP data (without FEC header).
	// IMPORTANT: must copy sendData before appending FEC header — sendData may
	// point into a pooled LZ4 buffer, and append's aliasing could corrupt it.
	rawForFEC := sendData
	if fecEnc != nil && t.fecEnabled() {
		gid, seq := fecEnc.CurrentGroupInfo()
		var fecHeader [5]byte
		binary.LittleEndian.PutUint32(fecHeader[:4], gid)
		fecHeader[4] = seq
		tmp := make([]byte, len(sendData)+5)
		copy(tmp, sendData)
		copy(tmp[len(sendData):], fecHeader[:])
		sendData = tmp
		flags |= protocol.DataFlagHasFEC
	}

	srcNet := net.IP(srcIP[:])
	dstNet := net.IP(dstIP[:])

	// Fast path: check server destination first (most common for relay)
	if dstKey == serverIPKey {
		t.sendToServerFEC(sendData, rawForFEC, srcNet, dstNet, encCipher, flags, fecEnc, token, serverAddr)
		return
	}

	// Broadcast/multicast: relay to all peers via server
	if cachedSubnet != nil && netutil.IsRelayTarget(dstNet, cachedSubnet) {
		t.sendToServerFEC(sendData, rawForFEC, srcNet, dstNet, encCipher, flags, fecEnc, token, serverAddr)
		return
	}

	if peerDirect {
		// P2P direct path — send directly for low latency.
		var packet []byte
		if p2pCipher != nil {
			packet = buildEncryptedDataPacket(srcNet, dstNet, sendData, p2pCipher, flags, token)
		} else {
			packet = buildDataPacket(srcNet, dstNet, sendData, flags, token)
		}
		t.sendUDP(packet, peerAddr)
		// Generate FEC parity for P2P path — use raw IP data without FEC header
		t.feedFEC(rawForFEC, peerAddr, fecEnc)
	} else {
		// Fallback: relay through server.
		t.sendToServerFEC(sendData, rawForFEC, srcNet, dstNet, encCipher, flags, fecEnc, token, serverAddr)
	}
}

// sendToServerFEC sends a packet via server relay and generates FEC parity.
// data is the protocol payload (with FEC header if enabled).
// rawForFEC is the raw IP data without FEC header, used for FEC encoding.
// serverAddr is passed explicitly (snapshotted under lock) to avoid data races
// with Connect() reassigning t.serverAddr.
func (t *Tunnel) sendToServerFEC(data, rawForFEC []byte, srcIP, dstIP net.IP, cipher *crypto.Cipher, flags byte, fecEnc *netutil.FECEncoder, token [16]byte, serverAddr *net.UDPAddr) {
	var packet []byte
	if cipher != nil {
		packet = buildEncryptedDataPacket(srcIP, dstIP, data, cipher, flags, token)
	} else {
		packet = buildDataPacket(srcIP, dstIP, data, flags, token)
	}
	t.sendUDP(packet, serverAddr)
	t.feedFEC(rawForFEC, serverAddr, fecEnc)
}

// feedFEC feeds raw IP data to the FEC encoder and sends any generated
// parity packet to the same destination.
func (t *Tunnel) feedFEC(data []byte, dest *net.UDPAddr, fecEnc *netutil.FECEncoder) {
	if fecEnc == nil {
		return
	}
	parity := fecEnc.Encode(data)
	if parity != nil {
		t.sendUDP(parity, dest)
	}
}
