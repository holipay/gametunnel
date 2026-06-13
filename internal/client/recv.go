package client

import (
	"context"
	"encoding/binary"
	"log"
	"net"
	"time"

	"github.com/holipay/gametunnel/internal/crypto"
	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/protocol"
)

// maxConsecutiveErrors is the number of consecutive read errors before
// a goroutine gives up. Prevents CPU spin on dead TUN/UDP devices.
const maxConsecutiveErrors = 10

// errorBackoff is the sleep duration between consecutive read errors.
// Chosen to be long enough to break a spin loop but short enough that
// a transient glitch recovers quickly.
const errorBackoff = 100 * time.Millisecond

// readBufSize is the buffer size for UDP and TUN reads.
// 4096 covers typical MTU (1400) + protocol overhead with headroom.
const readBufSize = 4096

// receiveFromServer handles packets from the server and direct P2P peers.
// It distinguishes between server-relayed packets and direct peer packets
// by checking the source address, which is critical for P2P detection.
func (t *Tunnel) receiveFromServer(ctx context.Context) {
	buf := make([]byte, readBufSize)
	consecutiveErrors := 0

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, from, err := t.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}

			consecutiveErrors++
			if consecutiveErrors > maxConsecutiveErrors {
				log.Printf(i18n.T().LogReadConsecFail, consecutiveErrors, err)
				return
			}

			// Backoff to avoid CPU spin on persistent errors.
			// Also gives ctx a chance to be checked.
			time.Sleep(errorBackoff)
			continue
		}

		// Successful read — reset error counter.
		consecutiveErrors = 0

		msg, err := protocol.DecodeChecked(buf[:n])
		if err != nil {
			continue
		}

		// Distinguish server-relayed vs direct P2P by source address.
		// Direct peer packets arrive from the peer's public address;
		// server-relayed packets arrive from the server's address.
		//
		// When the server listens on a wildcard address (0.0.0.0 or [::]),
		// from.IP won't match serverAddr.IP. Fall back to port matching
		// for loopback (covers localhost testing) and reject non-loopback
		// to avoid misclassifying peer packets as server traffic.
		fromServer := from != nil && t.serverAddr != nil && from.IP.Equal(t.serverAddr.IP)
		if !fromServer && t.serverAddr != nil && t.serverAddr.IP.IsUnspecified() {
			fromServer = from.Port == t.serverAddr.Port && from.IP.IsLoopback()
		}
		if fromServer {
			// Server-relayed packet
			t.handleServerData(ctx, msg)
		} else if from != nil && t.serverAddr != nil {
			// Direct P2P packet from a peer's public address
			t.handleDirectData(from, msg)
		}
	}
}

// handleServerData dispatches server-relayed protocol messages.
func (t *Tunnel) handleServerData(ctx context.Context, msg *protocol.Message) {
	// Any data from the server confirms it's alive
	t.markServerResponse()

	switch msg.Type {
	case protocol.TypePeerInfo:
		t.handlePeerInfo(ctx, msg.Payload)
	case protocol.TypeData:
		t.handleDataFromServer(msg.Payload)
	case protocol.TypePing:
		t.sendCtrl(protocol.EncodeChecked(protocol.TypePong, msg.Payload), t.serverAddr)
	case protocol.TypeHolePunch:
		t.handleHolePunchReceived(ctx, msg.Payload)
	}
}

// handleDirectData processes a packet received directly from a peer's public
// address (not via the server relay). Only TypeData is expected on this path.
// This is the ONLY place DirectReach should be set — it confirms actual P2P
// connectivity because the packet arrived from the peer's real address.
func (t *Tunnel) handleDirectData(from *net.UDPAddr, msg *protocol.Message) {
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

	t.mu.RLock()
	dev := t.tunDev
	t.mu.RUnlock()
	if dev == nil {
		protocol.PutDataPayload(dp)
		return
	}

	// Validate srcIP is a known peer (anti-spoofing)
	srcKey := ipKey(dp.SrcIP)
	t.mu.RLock()
	peer, known := t.peers[srcKey]
	t.mu.RUnlock()
	if !known {
		protocol.PutDataPayload(dp)
		return
	}

	// Verify the packet actually came from this peer's public address (IP + port)
	if peer.PublicAddr == nil || !from.IP.Equal(peer.PublicAddr.IP) || from.Port != peer.PublicAddr.Port {
		protocol.PutDataPayload(dp)
		return
	}

	// Mark P2P direct path confirmed — this is the legitimate DirectReach signal
	peer.DirectReach.Store(true)

	// Decrypt if encrypted — P2P uses p2pCipher (DirClientToClient),
	// not decCipher (DirServerToClient) which is for relay path only.
	outData := dp.Data
	if t.p2pCipher != nil && crypto.IsEncrypted(dp.Data) {
		outData, err = t.p2pCipher.Decrypt(dp.Data)
		if err != nil {
			protocol.PutDataPayload(dp)
			return
		}
	}

	if _, err := dev.Write(outData); err != nil {
		log.Printf(i18n.T().LogTUNWriteFail, err)
	}
	protocol.PutDataPayload(dp)
}

// handlePeerInfo updates the peer list from the server.
func (t *Tunnel) handlePeerInfo(ctx context.Context, payload []byte) {
	info, err := protocol.UnmarshalPeerInfo(payload)
	if err != nil {
		return
	}

	var newPeerIPs []net.IP // peers that need hole punching
	var changedPeerIPs []net.IP // peers whose public address changed (need re-punch)
	now := time.Now()

	t.mu.Lock()

	newPeers := make(map[[16]byte]*Peer, len(info.Peers))
	for _, entry := range info.Peers {
		// Skip self — server sends full list including this client
		if entry.VirtualIP.Equal(t.virtualIP) {
			continue
		}
		key := ipKey(entry.VirtualIP)
		if existing, ok := t.peers[key]; ok {
			// Check if peer's public address changed (NAT rebinding)
			addrChanged := existing.PublicAddr != nil && entry.PublicAddr != nil &&
				(!existing.PublicAddr.IP.Equal(entry.PublicAddr.IP) || existing.PublicAddr.Port != entry.PublicAddr.Port)
			if addrChanged {
				log.Printf(i18n.T().LogPeerAddrChange, entry.Username, entry.VirtualIP, existing.PublicAddr, entry.PublicAddr)
				existing.DirectReach.Store(false) // reset P2P status, need re-punch
				changedPeerIPs = append(changedPeerIPs, entry.VirtualIP)
			}
			existing.PublicAddr = entry.PublicAddr
			existing.Username = entry.Username
			existing.lastSeen.Store(&now)
			newPeers[key] = existing
		} else {
			p := &Peer{
				VirtualIP:  entry.VirtualIP,
				PublicAddr: entry.PublicAddr,
				Username:   entry.Username,
			}
			p.lastSeen.Store(&now)
			newPeers[key] = p
			log.Printf(i18n.T().LogNewPeer, entry.Username, entry.VirtualIP)
			newPeerIPs = append(newPeerIPs, entry.VirtualIP)
		}
	}
	// Log removed peers
	for key, peer := range t.peers {
		if _, ok := newPeers[key]; !ok {
			log.Printf(i18n.T().LogPeerLeave2, peer.Username, peer.VirtualIP)
		}
	}
	t.peers = newPeers

	t.mu.Unlock()

	// Launch hole punches outside the lock to avoid holding it during goroutine creation
	for _, peerIP := range newPeerIPs {
		go t.startHolePunch(ctx, peerIP)
	}
	// Re-punch peers whose address changed (NAT rebinding)
	for _, peerIP := range changedPeerIPs {
		go t.startHolePunch(ctx, peerIP)
	}
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

	t.mu.RLock()
	dev := t.tunDev
	t.mu.RUnlock()
	if dev == nil {
		protocol.PutDataPayload(dp)
		return
	}

	srcKey := ipKey(dp.SrcIP)

	// Allow traffic from the server's virtual IP (relay path) or known peers.
	if srcKey != t.serverIPKey {
		t.mu.RLock()
		_, known := t.peers[srcKey]
		t.mu.RUnlock()
		if !known {
			// Unknown srcIP — drop to prevent injection.
			protocol.PutDataPayload(dp)
			return
		}
	}

	// Decrypt if encrypted
	outData := dp.Data
	if t.decCipher != nil && crypto.IsEncrypted(dp.Data) {
		outData, err = t.decCipher.Decrypt(dp.Data)
		if err != nil {
			// Decrypt failure — drop packet (tampered or wrong key)
			protocol.PutDataPayload(dp)
			return
		}
	}

	if _, err := dev.Write(outData); err != nil {
		log.Printf(i18n.T().LogTUNWriteFail, err)
	}
	protocol.PutDataPayload(dp)
}

// receiveFromTUN reads IP packets from the TUN device and dispatches them
// to tunWorker goroutines for routing. The reader only does lightweight
// validation (IPv4 header check) and copies the packet into a new buffer
// before dispatching — the TUN read buffer is reused immediately.
func (t *Tunnel) receiveFromTUN(ctx context.Context) {
	buf := make([]byte, readBufSize)
	consecutiveErrors := 0

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		t.mu.RLock()
		dev := t.tunDev
		t.mu.RUnlock()
		if dev == nil {
			return
		}
		n, err := dev.Read(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}

			consecutiveErrors++
			if consecutiveErrors > maxConsecutiveErrors {
				log.Printf(i18n.T().LogTUNConsecFail, consecutiveErrors, err)
				return
			}

			time.Sleep(errorBackoff)
			continue
		}

		consecutiveErrors = 0

		if n < 20 {
			continue
		}

		// Validate IPv4 header
		if buf[0]>>4 != 4 {
			continue
		}
		ihl := int(buf[0]&0x0F) * 4
		if ihl < 20 || n < ihl {
			continue
		}
		// Validate IP total length matches actual read length
		totalLen := int(binary.BigEndian.Uint16(buf[2:4]))
		if totalLen < ihl || totalLen > n {
			continue
		}

		srcIP := net.IP(buf[12:16])
		dstIP := net.IP(buf[16:20])

		// Copy packet data — buf is reused on the next Read, but workers
		// process packets asynchronously. For game packets (60-1500 bytes)
		// the copy cost is negligible compared to the benefit of parallel
		// encryption and UDP send.
		pkt := make([]byte, n)
		copy(pkt, buf[:n])

		select {
		case t.tunCh <- tunJob{data: pkt, srcIP: srcIP, dstIP: dstIP}:
		default:
			// Worker channel full — drop packet (backpressure)
		}
	}
}
