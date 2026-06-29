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
func (t *Tunnel) receiveFromServer(ctx context.Context, conn *net.UDPConn) {
	buf := make([]byte, readBufSize)
	consecutiveErrors := 0

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, from, err := conn.ReadFromUDP(buf)
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

		// Check for FEC parity packets first (raw, not protocol-wrapped).
		// FEC packets are raw byte sequences that fail protocol decoding,
		// so this check must precede DecodeLenient/DecodeSkipCRC.
		if n >= netutil.FECHeaderSize && netutil.IsFECPacket(buf[:n]) {
			t.handleFECPacket(buf[:n])
			continue
		}

		// Encrypted rooms skip CRC32 (AEAD provides integrity).
		t.mu.RLock()
		encrypted := t.decCipher != nil
		t.mu.RUnlock()
		var msg *protocol.Message
		if encrypted {
			msg, err = protocol.DecodeSkipCRC(buf[:n])
		} else {
			msg, err = protocol.DecodeChecked(buf[:n])
		}
		if err != nil {
			continue
		}

		sa := t.serverAddr.Load()
		fromServer := from != nil && sa != nil && from.IP.Equal(sa.IP) && from.Port == sa.Port

		// Secondary heuristic: server-only message types are definitely
		// from the server. Catches the race window during reconnect where
		// t.serverAddr may have been updated but the packet arrived on
		// the old connection.
		if !fromServer {
			fromServer = msg.Type == protocol.TypePeerInfo ||
				msg.Type == protocol.TypePing ||
				msg.Type == protocol.TypeRebindAck ||
				msg.Type == protocol.TypeKick
		}

		// Strip trailing CRC for encrypted relay data from older servers
		// that still append the redundant CRC. New servers (v1.8+) omit
		// it because AEAD already provides integrity. The version check
		// avoids depending on fromServer being correct.
		if encrypted && msg.Type == protocol.TypeData && len(msg.Payload) >= protocol.ChecksumLen {
			if t.serverVersion.Load() < uint32(protocol.MinRelayNoCRCVersion) {
				msg.Payload = msg.Payload[:len(msg.Payload)-protocol.ChecksumLen]
			}
		}

		if fromServer {
			t.handleServerData(ctx, conn, msg)
		} else if from != nil && sa != nil {
			t.handleDirectData(ctx, from, msg)
		}
	}
}

// handleServerData dispatches server-relayed protocol messages.
// conn is the UDP connection from receiveFromServer — used instead of t.conn
// to avoid races with Connect() replacing t.conn after this goroutine started.
func (t *Tunnel) handleServerData(ctx context.Context, conn *net.UDPConn, msg *protocol.Message) {
	// Any data from the server confirms it's alive
	t.markServerResponse()

	switch msg.Type {
	case protocol.TypePeerInfo:
		t.handlePeerInfo(ctx, msg.Payload)
	case protocol.TypeData:
		t.handleDataFromServer(msg.Payload)
	case protocol.TypePing:
		t.sendCtrl(protocol.EncodeChecked(protocol.TypePong, msg.Payload), t.serverAddr.Load())
	case protocol.TypeHolePunch:
		t.handleHolePunchReceived(ctx, msg.Payload)
	case protocol.TypeNATResponse:
		// NAT probe response — handled by ProbeNATType via direct read, ignore here
	case protocol.TypeRebindAck:
		ack, err := protocol.UnmarshalRebindAck(msg.Payload)
		if err == nil {
			// Non-blocking send — if tryRebind isn't waiting, drop it
			select {
			case t.rebindAckCh <- ack:
			default:
			}
		}
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
		if conn != nil {
			conn.Close()
		}
	}
}

// decryptWriteAndRelease decrypts data (if encrypted), decompresses (if
// compressed), and writes to TUN device. Releases the DataPayload back to
// the pool when done.
func (t *Tunnel) decryptWriteAndRelease(dp *protocol.DataPayload, cipher *crypto.Cipher) {
	t.mu.RLock()
	dev, _ := t.tunDev.Load().(TunDevice)
	lz4Dec := t.lz4Decoder
	t.mu.RUnlock()
	if dev == nil {
		protocol.PutDataPayload(dp)
		return
	}

	outData := dp.Data
	if cipher != nil && crypto.IsEncrypted(dp.Data) {
		// Use pooled buffer for decryption output to reduce GC pressure
		decBuf := netutil.PktBufGet(len(dp.Data))
		var err error
		outData, err = cipher.DecryptInto(decBuf[:0], dp.Data)
		if err != nil {
			netutil.PktBufPut(decBuf)
			protocol.PutDataPayload(dp)
			return
		}
		// Note: decBuf may be returned to pool after outData is consumed
		defer netutil.PktBufPut(decBuf)
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
		defer lz4Dec.PutBuffer(decompressed)
	}

	if _, err := dev.Write(outData); err != nil {
		log.Printf(i18n.T().LogTUNWriteFail, err)
	}
	protocol.PutDataPayload(dp)
}

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
	p2pCipher := t.p2pCipher
	fecDec := t.fecDecoder
	dev, _ := t.tunDev.Load().(TunDevice)
	lz4Dec := t.lz4Decoder

	// Validate srcIP is a known peer (anti-spoofing)
	srcKey := ipKey(dp.SrcIP)
	peer, known := t.peers[srcKey]

	// Snapshot session token for unencrypted P2P auth
	sessionToken := t.sessionToken
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
		if len(msg.Payload) < 25 || msg.Payload[8]&protocol.DataFlagHasToken == 0 {
			protocol.PutDataPayload(dp)
			return
		}
		var pktToken [16]byte
		copy(pktToken[:], msg.Payload[9:25])
		if pktToken != sessionToken {
			protocol.PutDataPayload(dp)
			return
		}
	}

	// Mark P2P direct path confirmed — this is the legitimate DirectReach signal
	peer.DirectReach.Store(true)

	if dev == nil {
		protocol.PutDataPayload(dp)
		return
	}

	// ── Step 1: Decrypt (if encrypted) ──
	// Must decrypt first because the FEC header is embedded INSIDE the
	// encrypted payload on the P2P path. Operating on ciphertext would
	// read garbage as groupID/seq and corrupt the AEAD by truncation.
	outData := dp.Data
	if p2pCipher != nil && crypto.IsEncrypted(dp.Data) {
		decBuf := netutil.PktBufGet(len(dp.Data))
		var decErr error
		outData, decErr = p2pCipher.DecryptInto(decBuf[:0], dp.Data)
		if decErr != nil {
			netutil.PktBufPut(decBuf)
			protocol.PutDataPayload(dp)
			return
		}
		defer netutil.PktBufPut(decBuf)
	}

	// ── Step 2: FEC recovery (operates on plaintext data) ──
	if protocol.IsFECEnabled(dp.Flags) && len(outData) >= protocol.FECHeaderSize && t.fecEnabled() {
		off := len(outData) - protocol.FECHeaderSize
		groupID := binary.LittleEndian.Uint32(outData[off:])
		seq := outData[off+4]
		rawData := outData[:off]
		outData = rawData

		if fecDec != nil {
			recovered := fecDec.ProcessDataPacket(groupID, seq, rawData)
			for _, pkt := range recovered {
				if len(pkt) >= 20 {
					out := pkt
					decompressed := false
					if protocol.IsCompressed(dp.Flags) && lz4Dec != nil {
						if d, err := lz4Dec.Decompress(pkt); err == nil {
							out = d
							decompressed = true
						}
					}
					if _, werr := dev.Write(out); werr != nil {
						log.Printf("[fec] recovered packet write error: %v", werr)
					}
					if decompressed {
						lz4Dec.PutBuffer(out)
					}
				}
				netutil.PktBufPut(pkt)
			}
		}
	}

	// ── Step 3: Decompress (if compressed) ──
	if protocol.IsCompressed(dp.Flags) && lz4Dec != nil {
		decompressed, decErr := lz4Dec.Decompress(outData)
		if decErr != nil {
			log.Printf("[lz4] decompress error: %v", decErr)
			protocol.PutDataPayload(dp)
			return
		}
		outData = decompressed
		defer lz4Dec.PutBuffer(decompressed)
	}

	// ── Step 4: Write to TUN ──
	if _, werr := dev.Write(outData); werr != nil {
		log.Printf(i18n.T().LogTUNWriteFail, werr)
	}
	protocol.PutDataPayload(dp)
}

// handleDirectHolePunch processes a TypeHolePunch received directly from a peer.
// Confirms direct reachability and triggers a punch-back response.
func (t *Tunnel) handleDirectHolePunch(ctx context.Context, from *net.UDPAddr, msg *protocol.Message) {
	if len(msg.Payload) < 4 {
		return
	}
	peerIP := net.IP(append([]byte(nil), msg.Payload[:4]...))

	t.mu.RLock()
	peer, ok := t.peers[ipKey(peerIP)]
	if !ok || peer.PublicAddr.Load() == nil {
		t.mu.RUnlock()
		return
	}
	peerAddr := peer.PublicAddr.Load()
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
	t.holePunchWg.Add(1)
	go func() {
		defer t.holePunchWg.Done()
		t.burstHolePunch(peerAddr, holePunchBurstPerPhase, 50*time.Millisecond, ctx)
	}()
}

// handleFECPacket processes an incoming FEC parity packet.
// Extracts the parity data and feeds it to the FEC decoder.
// If the decoder recovers any lost packets, they are written to TUN.
func (t *Tunnel) handleFECPacket(data []byte) {
	t.mu.RLock()
	fecDec := t.fecDecoder
	lz4Dec := t.lz4Decoder
	dev, _ := t.tunDev.Load().(TunDevice)
	fecEnabled := t.fecEnabled()
	t.mu.RUnlock()

	if fecDec == nil || dev == nil || !fecEnabled {
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
			out := pkt
			decompressed := false
			if lz4Dec != nil {
				if d, err := lz4Dec.Decompress(pkt); err == nil {
					out = d
					decompressed = true
				}
			}
			if _, err := dev.Write(out); err != nil {
				log.Printf("[fec] recovered packet write error: %v", err)
			}
			if decompressed {
				lz4Dec.PutBuffer(out)
			}
		}
		netutil.PktBufPut(pkt)
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
			existingAddr := existing.PublicAddr.Load()
			addrChanged := existingAddr != nil && pubAddr != nil &&
				(!existingAddr.IP.Equal(pubAddr.IP) || existingAddr.Port != pubAddr.Port)
			if addrChanged {
				log.Printf(i18n.T().LogPeerAddrChange, entry.Username, entry.VirtualIP, existing.PublicAddr.Load(), entry.PublicAddr)
				existing.DirectReach.Store(false) // reset P2P status, need re-punch
				changedPeerIPs = append(changedPeerIPs, entry.VirtualIP)
			}
			existing.PublicAddr.Store(pubAddr)
			existing.Username = entry.Username
			existing.lastSeen.Store(now.UnixNano())
			t.peers[key] = existing
		} else {
			p := &Peer{
				VirtualIP: entry.VirtualIP,
				Username:  entry.Username,
			}
			if pubAddr != nil {
				p.PublicAddr.Store(pubAddr)
			}
			p.lastSeen.Store(now.UnixNano())
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
// Decryption MUST happen before FEC processing: the FEC header is inside
// the encrypted payload and operating on ciphertext reads garbage as
// groupID/seq and corrupts AEAD by truncation.
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
	serverIPKey, _ := t.serverIPKey.Load().([16]byte)
	decCipher := t.decCipher
	_, known := t.peers[srcKey]
	fecDec := t.fecDecoder
	lz4Dec := t.lz4Decoder
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

	// ── Step 1: Decrypt (if encrypted) ──
	outData := dp.Data
	if decCipher != nil && crypto.IsEncrypted(dp.Data) {
		decBuf := netutil.PktBufGet(len(dp.Data))
		var decErr error
		outData, decErr = decCipher.DecryptInto(decBuf[:0], dp.Data)
		if decErr != nil {
			netutil.PktBufPut(decBuf)
			protocol.PutDataPayload(dp)
			return
		}
		defer netutil.PktBufPut(decBuf)
	}

	// ── Step 2: FEC recovery (operates on plaintext data) ──
	if protocol.IsFECEnabled(dp.Flags) && len(outData) >= protocol.FECHeaderSize && t.fecEnabled() {
		off := len(outData) - protocol.FECHeaderSize
		groupID := binary.LittleEndian.Uint32(outData[off:])
		seq := outData[off+4]
		rawData := outData[:off]
		outData = rawData

		if fecDec != nil {
			recovered := fecDec.ProcessDataPacket(groupID, seq, rawData)
			for _, pkt := range recovered {
				if len(pkt) >= 20 {
					out := pkt
					decompressed := false
					if protocol.IsCompressed(dp.Flags) && lz4Dec != nil {
						if d, err := lz4Dec.Decompress(pkt); err == nil {
							out = d
							decompressed = true
						}
					}
					if _, werr := dev.Write(out); werr != nil {
						log.Printf("[fec] recovered packet write error: %v", werr)
					}
					if decompressed {
						lz4Dec.PutBuffer(out)
					}
				}
				netutil.PktBufPut(pkt)
			}
		}
	}

	// ── Step 3: Decompress (if compressed) ──
	if protocol.IsCompressed(dp.Flags) && lz4Dec != nil {
		decompressed, decErr := lz4Dec.Decompress(outData)
		if decErr != nil {
			log.Printf("[lz4] decompress error: %v", decErr)
			protocol.PutDataPayload(dp)
			return
		}
		outData = decompressed
		defer lz4Dec.PutBuffer(decompressed)
	}

	// ── Step 4: Write to TUN ──
	if _, werr := dev.Write(outData); werr != nil {
		log.Printf(i18n.T().LogTUNWriteFail, werr)
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
		dev, _ := t.tunDev.Load().(TunDevice)
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

		// Extract src/dst IPs as [4]byte to avoid heap escape via channel send.
		// net.IP(srcIP[:]) in the worker goroutine stays on its stack.
		var srcIP, dstIP [4]byte
		copy(srcIP[:], buf[12:16])
		copy(dstIP[:], buf[16:20])

		// Copy packet data — buf is reused on the next Read, but workers
		// process packets asynchronously. Use pooled buffer to reduce
		// GC pressure on the hot path.
		pkt := netutil.PktBufGet(n)[:n]
		copy(pkt, buf[:n])

		select {
		case t.tunCh <- tunJob{data: pkt, srcIP: srcIP, dstIP: dstIP}:
		default:
			// Worker channel full — drop packet and return buffer to pool
			netutil.PktBufPut(pkt)
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
