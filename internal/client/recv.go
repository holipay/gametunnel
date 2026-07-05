package client

import (
	"context"
	"encoding/binary"
	"log"
	"net"
	"time"

	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/pool"
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
func (t *Tunnel) receiveFromServer(ctx context.Context, conn *net.UDPConn, serverAddr *net.UDPAddr) {
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

		// Encrypted rooms skip CRC32 (AEAD provides integrity).
		encrypted := t.crypto.decAvailable.Load()
		var msg *protocol.Message
		if encrypted {
			msg, err = protocol.DecodeSkipCRC(buf[:n])
		} else {
			msg, err = protocol.DecodeChecked(buf[:n])
		}
		if err != nil {
			continue
		}

		// Use the snapshot serverAddr (captured at Connect time) for the primary
		// fromServer check. This avoids the race window during reconnect where
		// t.serverAddr may have been updated but the packet arrived on the old
		// connection. The secondary heuristic (server-only message types) provides
		// additional protection for edge cases.
		fromServer := from != nil && from.IP.Equal(serverAddr.IP) && from.Port == serverAddr.Port

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

		// When client and server are on the same machine, the kernel may
		// route server responses through loopback (::1 or 127.0.0.1) even
		// when the client connected via the server's public IPv4/IPv6
		// address. The port check ensures this only matches the actual
		// server process.
		if !fromServer && from != nil && serverAddr != nil &&
			from.Port == serverAddr.Port && from.IP.IsLoopback() {
			fromServer = true
		}

		// Strip trailing CRC for encrypted relay data from older servers
		// that still append the redundant CRC. New servers (v1.8+) omit
		// it because AEAD already provides integrity. The version check
		// avoids depending on fromServer being correct.
		if encrypted && msg.Type == protocol.TypeData && len(msg.Payload) >= protocol.ChecksumLen {
			if t.session.serverVersion.Load() < uint32(protocol.MinRelayNoCRCVersion) {
				msg.Payload = msg.Payload[:len(msg.Payload)-protocol.ChecksumLen]
			}
		}

		if fromServer {
			t.handleServerData(ctx, conn, msg)
		} else if from != nil && serverAddr != nil {
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
			de := protocol.NewDisconnectError(kick)
			log.Printf("kicked by server: %s", de.Message)
			if de.IsFatal() {
				t.cancelKicks.Store(true)
			}
		}
		if conn != nil {
			conn.Close()
		}
	}
}


// receiveFromTUN reads IP packets from the TUN device and dispatches them
// to tunWorker goroutines for routing. Uses ReadBatch on Linux for reduced
// syscall overhead; falls back to single-packet Read on other platforms.
func (t *Tunnel) receiveFromTUN(ctx context.Context) {
	consecutiveErrors := 0

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		dev, _ := t.tunDev.Load().(TunDevice)
		if dev == nil {
			return
		}

		batchSize := dev.BatchSize()
		if batchSize <= 1 {
			// Fallback: single-packet read
			t.receiveFromTUNSingle(ctx, dev, &consecutiveErrors)
			return
		}

		// Batch read path
		bufs := make([][]byte, batchSize)
		sizes := make([]int, batchSize)
		for i := range bufs {
			bufs[i] = pool.PktBufGet(readBufSize)
		}

		n, err := dev.ReadBatch(bufs, sizes)
		if err != nil {
			// Return all pooled buffers before handling error
			for i := range bufs {
				pool.PktBufPut(bufs[i])
				bufs[i] = nil
			}
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

		for i := 0; i < n; i++ {
			pkt := bufs[i][:sizes[i]]
			if sizes[i] < 20 || pkt[0]>>4 != 4 {
				pool.PktBufPut(pkt)
				continue
			}
			ihl := int(pkt[0]&0x0F) * 4
			if ihl < 20 || sizes[i] < ihl {
				pool.PktBufPut(pkt)
				continue
			}
			totalLen := int(binary.BigEndian.Uint16(pkt[2:4]))
			if totalLen < ihl || totalLen > sizes[i] {
				pool.PktBufPut(pkt)
				continue
			}

			var srcIP, dstIP [4]byte
			copy(srcIP[:], pkt[12:16])
			copy(dstIP[:], pkt[16:20])

			// Trim to actual IP length
			pkt = pkt[:totalLen]

			select {
			case t.tunCh <- tunJob{data: pkt, srcIP: srcIP, dstIP: dstIP}:
			default:
				pool.PktBufPut(pkt)
				n := t.tunDropped.Add(1)
				if n&(n-1) == 0 {
					log.Printf("[tunnel] TUN channel full, %d packets dropped total", n)
				}
			}
		}
	}
}

// receiveFromTUNSingle is the fallback single-packet read path.
func (t *Tunnel) receiveFromTUNSingle(ctx context.Context, dev TunDevice, consecutiveErrors *int) {
	buf := make([]byte, readBufSize)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := dev.Read(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}

			*consecutiveErrors++
			if *consecutiveErrors > maxConsecutiveErrors {
				log.Printf(i18n.T().LogTUNConsecFail, *consecutiveErrors, err)
				return
			}

			time.Sleep(errorBackoff)
			continue
		}

		*consecutiveErrors = 0

		if n < 20 {
			continue
		}

		if buf[0]>>4 != 4 {
			continue
		}
		ihl := int(buf[0]&0x0F) * 4
		if ihl < 20 || n < ihl {
			continue
		}
		totalLen := int(binary.BigEndian.Uint16(buf[2:4]))
		if totalLen < ihl || totalLen > n {
			continue
		}

		var srcIP, dstIP [4]byte
		copy(srcIP[:], buf[12:16])
		copy(dstIP[:], buf[16:20])

		pkt := pool.PktBufGet(n)[:n]
		copy(pkt, buf[:n])

		select {
		case t.tunCh <- tunJob{data: pkt, srcIP: srcIP, dstIP: dstIP}:
		default:
			pool.PktBufPut(pkt)
			n := t.tunDropped.Add(1)
			if n&(n-1) == 0 {
				log.Printf("[tunnel] TUN channel full, %d packets dropped total", n)
			}
		}
	}
}
