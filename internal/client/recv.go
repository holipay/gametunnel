package client

import (
	"context"
	"encoding/binary"
	"log"
	"net"
	"strings"
	"time"

	"github.com/holipay/gametunnel/internal/crypto"
	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/netutil"
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
// 65535 covers max UDP datagram size, reducing read truncation under load.
const readBufSize = 65535

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

		// Encrypted rooms skip CRC32 (AEAD provides integrity).
		t.mu.RLock()
		encrypted := t.decCipher != nil
		t.mu.RUnlock()
		var msg *protocol.Message
		if encrypted {
			msg, err = protocol.DecodeSkipCRC(buf[:n])
		} else {
			msg, err = protocol.DecodeLenient(buf[:n])
		}
		if err != nil {
			continue
		}

		fromServer := from != nil && t.serverAddr != nil && from.IP.Equal(t.serverAddr.IP) && from.Port == t.serverAddr.Port

		// Check for FEC parity packets (raw, not wrapped in protocol message)
		if n >= netutil.FECHeaderSize && netutil.IsFECPacket(buf[:n]) {
			t.handleFECPacket(buf[:n])
			continue
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
	case protocol.TypeNATResponse:
		// NAT probe response — handled by ProbeNATType via direct read, ignore here
	case protocol.TypeRebindAck:
		// Rebind ack — handled by tryRebind via direct read, ignore here
	case protocol.TypeKick:
		kick, err := protocol.UnmarshalKick(msg.Payload)
		if err == nil {
			log.Printf("kicked by server: %s", kick.Reason)
		}
		// For non-recoverable kicks (wrong password, version mismatch),
		// cancel context to stop the reconnect loop.
		if err == nil && isFatalKick(kick) {
			t.cancelKicks.Store(true)
		}
		if t.conn != nil {
			t.conn.Close()
		}
	}
}

// decryptWriteAndRelease decrypts data (if encrypted), decompresses (if
// compressed), and writes to TUN device. Releases the DataPayload back to
// the pool when done.
func (t *Tunnel) decryptWriteAndRelease(dp *protocol.DataPayload, cipher *crypto.Cipher) {
	t.mu.RLock()
	dev := t.tunDev
	lz4Dec := t.lz4Decoder
	t.mu.RUnlock()
	if dev == nil {
		protocol.PutDataPayload(dp)
		return
	}

	outData := dp.Data
	if cipher != nil && crypto.IsEncrypted(dp.Data) {
		var err error
		outData, err = cipher.Decrypt(dp.Data)
		if err != nil {
			protocol.PutDataPayload(dp)
			return
		}
	}

	// Decompress if LZ4 flag is set
	if protocol.IsCompressed(dp.Flags) && lz4Dec != nil {
		decompressed, err := lz4Dec.Decompress(outData)
		if err != nil {
			log.Printf("[lz4] decompress error: %v", err)
			protocol.PutDataPayload(dp)
			return
		}
		outData = decompressed
	}

	if _, err := dev.Write(outData); err != nil {
		log.Printf(i18n.T().LogTUNWriteFail, err)
	}
	protocol.PutDataPayload(dp)
}

func (t *Tunnel) handleDirectData(from *net.UDPAddr, msg *protocol.Message) {
	if msg.Type == protocol.TypeHolePunch {
		t.handleDirectHolePunch(from, msg)
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

	// Decrypt (P2P uses p2pCipher) and write to TUN
	t.mu.RLock()
	cipher := t.p2pCipher
	t.mu.RUnlock()
	t.decryptWriteAndRelease(dp, cipher)
}

// handleDirectHolePunch processes a TypeHolePunch received directly from a peer.
// Confirms direct reachability and triggers a punch-back response.
func (t *Tunnel) handleDirectHolePunch(from *net.UDPAddr, msg *protocol.Message) {
	if len(msg.Payload) < 4 {
		return
	}
	peerIP := net.IP(append([]byte(nil), msg.Payload[:4]...))

	t.mu.RLock()
	peer, ok := t.peers[ipKey(peerIP)]
	if !ok || peer.PublicAddr == nil {
		t.mu.RUnlock()
		return
	}
	peerAddr := peer.PublicAddr
	t.mu.RUnlock()

	// Verify the sender matches the peer's known public address (anti-spoofing)
	if !from.IP.Equal(peerAddr.IP) || from.Port != peerAddr.Port {
		return
	}

	// Rate limit: at most once per holePunchBackoff per peer
	if !peer.tryRateLimitHolePunch(holePunchBackoff) {
		return
	}

	// Mark direct path confirmed — received a packet directly from the peer
	peer.DirectReach.Store(true)

	// Punch back in a goroutine — don't block the receive loop
	go func() {
		t.burstHolePunch(peerAddr, holePunchBurstPerPhase, 50*time.Millisecond, context.Background())
	}()
}

// handleFECPacket processes an incoming FEC parity packet.
// Extracts the parity data and feeds it to the FEC decoder.
// If the decoder recovers any lost packets, they are written to TUN.
func (t *Tunnel) handleFECPacket(data []byte) {
	t.mu.RLock()
	fecDec := t.fecDecoder
	dev := t.tunDev
	t.mu.RUnlock()

	if fecDec == nil || dev == nil {
		return
	}

	groupID, groupSize, err := netutil.ParseFECHeader(data)
	if err != nil {
		return
	}
	parity := netutil.ParseFECParity(data)
	if parity == nil {
		return
	}

	recovered := fecDec.ProcessParityPacket(groupID, groupSize, parity)
	for _, pkt := range recovered {
		if len(pkt) >= 20 {
			// Write recovered packet to TUN
			if _, err := dev.Write(pkt); err != nil {
				log.Printf("[fec] recovered packet write error: %v", err)
			}
		}
	}
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

	// Build a fresh map instead of clearing t.peers in-place.
	// oldPeers and t.peers MUST be different maps so that looking
	// up existing peers in oldPeers works correctly.
	oldPeers := t.peers
	if oldPeers == nil {
		oldPeers = make(map[[16]byte]*Peer)
	}
	t.peers = make(map[[16]byte]*Peer, len(info.Peers))
	for _, entry := range info.Peers {
		// Skip self — server sends full list including this client
		if entry.VirtualIP.Equal(t.virtualIP) {
			continue
		}
		key := ipKey(entry.VirtualIP)
		// Normalize PublicAddr.IP to 16 bytes (IPv4 → ::ffff:x.x.x.x) so
		// that IP comparisons with addresses received on the IPv6 socket
		// (always 16 bytes) work correctly. Required for fromServer check
		// in receiveFromServer and P2P detection in handleDirectData.
		pubAddr := entry.PublicAddr
		if pubAddr != nil {
			if ip16 := pubAddr.IP.To16(); ip16 != nil {
				pubAddr = &net.UDPAddr{IP: ip16, Port: pubAddr.Port}
			}
		}
		if existing, ok := oldPeers[key]; ok {
			// Check if peer's public address changed (NAT rebinding)
			addrChanged := existing.PublicAddr != nil && pubAddr != nil &&
				(!existing.PublicAddr.IP.Equal(pubAddr.IP) || existing.PublicAddr.Port != pubAddr.Port)
			if addrChanged {
				log.Printf(i18n.T().LogPeerAddrChange, entry.Username, entry.VirtualIP, existing.PublicAddr, entry.PublicAddr)
				existing.DirectReach.Store(false) // reset P2P status, need re-punch
				changedPeerIPs = append(changedPeerIPs, entry.VirtualIP)
			}
			existing.PublicAddr = pubAddr
			existing.Username = entry.Username
			existing.lastSeen.Store(&now)
			t.peers[key] = existing
		} else {
			p := &Peer{
				VirtualIP:  entry.VirtualIP,
				PublicAddr: pubAddr,
				Username:   entry.Username,
			}
			p.lastSeen.Store(&now)
			t.peers[key] = p
			log.Printf(i18n.T().LogNewPeer, entry.Username, entry.VirtualIP)
			newPeerIPs = append(newPeerIPs, entry.VirtualIP)
		}
	}
	// Log removed peers (those in oldPeers but not in the updated t.peers)
	for key, peer := range oldPeers {
		if _, ok := t.peers[key]; !ok {
			log.Printf(i18n.T().LogPeerLeave2, peer.Username, peer.VirtualIP)
		}
	}

	t.mu.Unlock()

	// Launch hole punches outside the lock to avoid holding it during goroutine creation
	allPeerIPs := append(newPeerIPs, changedPeerIPs...)
	for _, peerIP := range allPeerIPs {
		go t.startHolePunch(ctx, peerIP)
		t.sendHolePunchRelay(peerIP)
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

	srcKey := ipKey(dp.SrcIP)

	// Snapshot fields under read lock to avoid races with reconnect
	t.mu.RLock()
	serverIPKey := t.serverIPKey
	decCipher := t.decCipher
	t.mu.RUnlock()

	// Allow traffic from the server's virtual IP (relay path) or known peers.
	if srcKey != serverIPKey {
		t.mu.RLock()
		_, known := t.peers[srcKey]
		t.mu.RUnlock()
		if !known {
			// Unknown srcIP — drop to prevent injection.
			protocol.PutDataPayload(dp)
			return
		}
	}

	// Decrypt (relay uses decCipher) and write to TUN
	t.decryptWriteAndRelease(dp, decCipher)
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

		// Extract src/dst IPs. The [4]byte arrays avoid heap allocation for
		// the copy itself, but net.IP() conversion causes escape. This is
		// still cheaper than make(net.IP, 4) which always allocates.
		var srcIPBuf, dstIPBuf [4]byte
		copy(srcIPBuf[:], buf[12:16])
		copy(dstIPBuf[:], buf[16:20])
		srcIP := net.IP(srcIPBuf[:])
		dstIP := net.IP(dstIPBuf[:])

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

// isFatalKick returns true if the kick reason indicates a non-recoverable error
// that should stop the reconnect loop (e.g. wrong password, version mismatch).
// Uses numeric codes when available (newer servers), falls back to string
// matching for backward compatibility with older servers.
func isFatalKick(kick *protocol.KickPayload) bool {
	if kick.Code == protocol.KickCodeWrongPassword || kick.Code == protocol.KickCodeVersionMismatch {
		return true
	}
	if kick.Code != protocol.KickCodeNone {
		return false
	}
	reason := kick.Reason
	return strings.Contains(reason, "密码错误") ||
		strings.Contains(reason, "password") ||
		strings.Contains(reason, "版本不兼容") ||
		strings.Contains(reason, "incompatible")
}
