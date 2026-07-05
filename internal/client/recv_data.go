package client

import (
	"context"
	"encoding/binary"
	"log"
	"net"

	"github.com/holipay/gametunnel/internal/crypto"
	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/netkey"
	"github.com/holipay/gametunnel/internal/pool"
	"github.com/holipay/gametunnel/internal/protocol"
)

// rewriteBroadcast replaces 255.255.255.255 in the IP packet header with the
// subnet-directed broadcast (e.g. 10.10.0.255). Windows may not deliver limited
// broadcast packets arriving on a TUN adapter to game processes, but
// subnet-directed broadcasts work reliably. Returns the (possibly rewritten)
// packet, whether it was modified, and whether the caller should return the
// buffer to the pool after use.
func rewriteBroadcast(pkt []byte, subnet *net.IPNet) (out []byte, modified bool, pooled bool) {
	if len(pkt) < 20 || subnet == nil || pkt[0]>>4 != 4 || len(subnet.Mask) == 0 {
		return pkt, false, false
	}
	// IPv4 header: dst offset = 16, length = 4
	dstOff := 16
	if binary.BigEndian.Uint32(pkt[dstOff:dstOff+4]) != 0xFFFFFFFF {
		return pkt, false, false
	}
	subIP := subnet.IP.To4()
	if subIP == nil {
		return pkt, false, false
	}
	// Compute subnet broadcast: network IP | ~mask
	var bcast [4]byte
	for i := 0; i < 4; i++ {
		bcast[i] = subIP[i] | ^subnet.Mask[i]
	}
	// Use pooled buffer to reduce GC pressure on broadcast-heavy paths
	out = pool.PktBufGet(len(pkt))[:len(pkt)]
	copy(out, pkt)
	copy(out[dstOff:dstOff+4], bcast[:])
	// Recalculate IP header checksum (RFC 1071)
	out[10] = 0
	out[11] = 0
	ihl := int(out[0]&0x0F) * 4
	var sum uint32
	for i := 0; i < ihl; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(out[i : i+2]))
	}
	for sum>>16 != 0 {
		sum = (sum & 0xFFFF) + (sum >> 16)
	}
	binary.BigEndian.PutUint16(out[10:12], ^uint16(sum))
	return out, true, true
}

func (t *Tunnel) decryptWriteAndRelease(dp *protocol.DataPayload, cipher *crypto.Cipher) {
	dev, _ := t.tunDev.Load().(TunDevice)
	if dev == nil {
		protocol.PutDataPayload(dp)
		return
	}

	outData := dp.Data
	if cipher != nil && crypto.IsEncrypted(dp.Data) {
		decBuf := pool.PktBufGet(len(dp.Data))
		var err error
		outData, err = cipher.DecryptInto(decBuf[:0], dp.Data)
		if err != nil {
			pool.PktBufPut(decBuf)
			protocol.PutDataPayload(dp)
			return
		}
		defer pool.PktBufPut(decBuf)
	}

	// Rewrite limited broadcast (255.255.255.255) to subnet-directed
	// broadcast so Windows delivers it to game processes on the TUN adapter.
	// rewriteBroadcast returns a pooled buffer if rewrite was needed.
	rewritten, _, pooled := rewriteBroadcast(outData, t.session.cachedSubnet.Load())
	if _, err := dev.Write(rewritten); err != nil {
		log.Printf(i18n.T().LogTUNWriteFail, err)
	}
	// Return pooled buffer if rewrite allocated one
	if pooled {
		pool.PktBufPut(rewritten)
	}
	protocol.PutDataPayload(dp)
}

// handleDirectData handles data packets received directly from a P2P peer.
func (t *Tunnel) handleDirectData(ctx context.Context, from *net.UDPAddr, msg *protocol.Message) {
	if msg.Type == protocol.TypeHolePunch {
		t.handleDirectHolePunch(ctx, from, msg)
		return
	}
	if msg.Type != protocol.TypeData {
		return
	}

	dp, err := protocol.UnmarshalDataPooled(msg.Payload)
	if err != nil || len(dp.Data) == 0 {
		if dp != nil {
			protocol.PutDataPayload(dp)
		}
		return
	}

	// Snapshot all needed fields under a single read lock
	t.mu.RLock()
	p2pCipher := t.crypto.p2pCipher

	// Validate srcIP is a known peer (anti-spoofing)
	srcKey := netkey.IPKey(dp.SrcIP)
	peer, known := t.peers[srcKey]

	// Snapshot session token for unencrypted P2P auth
	sessionToken := t.session.sessionToken
	t.mu.RUnlock()

	if !known {
		protocol.PutDataPayload(dp)
		return
	}

	// Verify the packet actually came from this peer's public address (IP + port)
	peerAddr := peer.PublicAddr.Load()
	if peerAddr == nil || !from.IP.Equal(peerAddr.IP) || from.Port != peerAddr.Port {
		protocol.PutDataPayload(dp)
		return
	}

	// For unencrypted P2P rooms, validate the session token to prevent
	// packet forgery. Encrypted rooms get implicit auth from AEAD.
	// All clients in a room share the same token, distributed by the
	// server during registration. Old servers (pre-v1.7) have zero tokens.
	if p2pCipher == nil && sessionToken != [16]byte{} {
		if len(msg.Payload) <= 8 {
			protocol.PutDataPayload(dp)
			return
		}
		flags, tokenOff, _ := protocol.ParseDataHeader(msg.Payload)
		if flags&protocol.DataFlagHasToken == 0 || len(msg.Payload) < tokenOff+protocol.DataTokenLen {
			protocol.PutDataPayload(dp)
			return
		}
		var pktToken [protocol.DataTokenLen]byte
		copy(pktToken[:], msg.Payload[tokenOff:tokenOff+protocol.DataTokenLen])
		if pktToken != sessionToken {
			protocol.PutDataPayload(dp)
			return
		}
	}

	// Mark P2P direct path confirmed — this is the legitimate DirectReach signal
	peer.DirectReach.Store(true)

	t.decryptWriteAndRelease(dp, p2pCipher)
}

// handleDataFromServer writes server-relayed data to the TUN device.
// Note: this path is ALWAYS server-relayed — direct P2P packets are handled
// by handleDirectData instead. Do NOT mark DirectReach here.
func (t *Tunnel) handleDataFromServer(payload []byte) {
	dp, err := protocol.UnmarshalDataPooled(payload)
	if err != nil {
		return
	}
	if len(dp.Data) == 0 {
		protocol.PutDataPayload(dp)
		return
	}

	dev, _ := t.tunDev.Load().(TunDevice)
	// p2pCipher is immutable during a session (set once at registration),
	// safe to read without lock.
	// The server relays encrypted bytes transparently — the original client
	// encrypts with p2pCipher (DirClientToClient), and the server never
	// decrypts/re-encrypts. Use p2pCipher here, NOT decCipher (which is
	// reserved for server-originated control messages).
	p2pCipher := t.crypto.p2pCipher

	// Accept all server-relayed traffic unconditionally. The server already
	// validates anti-spoofing (srcIP must match the sender's registered virtual
	// IP), so the client can trust relayed packets without the known-peer check.
	// This prevents a race where game discovery broadcasts from a newly joined
	// peer arrive before the peer info broadcast reaches existing clients.

	if dev == nil {
		protocol.PutDataPayload(dp)
		return
	}

	t.decryptWriteAndRelease(dp, p2pCipher)
}
