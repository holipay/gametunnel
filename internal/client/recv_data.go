package client

import (
	"context"
	"log"
	"net"

	"github.com/holipay/gametunnel/internal/crypto"
	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/pool"
	"github.com/holipay/gametunnel/internal/protocol"
)

// decryptWriteAndRelease decrypts data (if encrypted), decompresses (if
// compressed), and writes to TUN device. Releases the DataPayload back to
// the pool when done.
func (t *Tunnel) decryptWriteAndRelease(dp *protocol.DataPayload, cipher *crypto.Cipher) {
	t.mu.RLock()
	dev, _ := t.tunDev.Load().(TunDevice)
	t.mu.RUnlock()
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

	if _, err := dev.Write(outData); err != nil {
		log.Printf(i18n.T().LogTUNWriteFail, err)
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
	srcKey := ipKey(dp.SrcIP)
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

	srcKey := ipKey(dp.SrcIP)

	// Snapshot all needed fields under a single read lock to avoid races with reconnect
	t.mu.RLock()
	serverIPKey, _ := t.session.serverIPKey.Load().([16]byte)
	decCipher := t.crypto.decCipher
	_, known := t.peers[srcKey]
	dev, _ := t.tunDev.Load().(TunDevice)
	t.mu.RUnlock()

	// Allow traffic from the server's virtual IP (relay path) or known peers.
	if srcKey != serverIPKey && !known {
		protocol.PutDataPayload(dp)
		return
	}

	if dev == nil {
		protocol.PutDataPayload(dp)
		return
	}

	t.decryptWriteAndRelease(dp, decCipher)
}
