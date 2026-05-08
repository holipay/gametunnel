package client

import (
	"context"
	"log"
	"net"
	"time"

	"github.com/holipay/gametunnel/internal/protocol"
)

// maxConsecutiveErrors is the number of consecutive read errors before
// a goroutine gives up. Prevents CPU spin on dead TUN/UDP devices.
const maxConsecutiveErrors = 10

// errorBackoff is the sleep duration between consecutive read errors.
// Chosen to be long enough to break a spin loop but short enough that
// a transient glitch recovers quickly.
const errorBackoff = 100 * time.Millisecond

// receiveFromServer handles packets from the server.
func (t *Tunnel) receiveFromServer(ctx context.Context) {
	buf := make([]byte, 65535)
	consecutiveErrors := 0

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, _, err := t.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}

			consecutiveErrors++
			if consecutiveErrors > maxConsecutiveErrors {
				log.Printf("[tunnel] 服务端连接读取连续失败 %d 次，退出: %v", consecutiveErrors, err)
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

		switch msg.Type {
		case protocol.TypePeerInfo:
			t.handlePeerInfo(ctx, msg.Payload)
		case protocol.TypeData:
			t.handleDataFromServer(msg.Payload)
		case protocol.TypePing:
			// Echo ping back as pong for RTT measurement
			t.sendUDP(protocol.EncodeChecked(protocol.TypePong, msg.Payload), t.serverAddr)
		case protocol.TypeHolePunch:
			// Bidirectional punch: respond immediately to create our NAT mapping
			t.handleHolePunchReceived(msg.Payload)
		}
	}
}

// handlePeerInfo updates the peer list from the server.
func (t *Tunnel) handlePeerInfo(ctx context.Context, payload []byte) {
	info, err := protocol.UnmarshalPeerInfo(payload)
	if err != nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	newPeers := make(map[[4]byte]*Peer, len(info.Peers))
	for _, entry := range info.Peers {
		// Skip self — server sends full list including this client
		if entry.VirtualIP.Equal(t.virtualIP) {
			continue
		}
		key := ip4Key(entry.VirtualIP)
		if existing, ok := t.peers[key]; ok {
			existing.PublicAddr = entry.PublicAddr
			existing.Username = entry.Username
			newPeers[key] = existing
		} else {
			newPeers[key] = &Peer{
				VirtualIP:  entry.VirtualIP,
				PublicAddr: entry.PublicAddr,
				Username:   entry.Username,
			}
			log.Printf("[tunnel] 新玩家: %s (%s)", entry.Username, entry.VirtualIP)
			go t.startHolePunch(ctx, entry.VirtualIP)
		}
	}
	// Log removed peers
	for key, peer := range t.peers {
		if _, ok := newPeers[key]; !ok {
			log.Printf("[tunnel] 玩家离开: %s (%s)", peer.Username, peer.VirtualIP)
		}
	}
	t.peers = newPeers
}

// handleDataFromServer writes relayed data to the TUN device.
func (t *Tunnel) handleDataFromServer(payload []byte) {
	dp, err := protocol.UnmarshalData(payload)
	if err != nil {
		return
	}
	if len(dp.Data) > 0 && t.tunDev != nil {
		// Track direct peer traffic for P2P path detection
		markDirectPeerTraffic(dp.SrcIP)
		t.tunDev.Write(dp.Data)
	}
}

// receiveFromTUN reads IP packets from the TUN device and routes them.
func (t *Tunnel) receiveFromTUN(ctx context.Context) {
	buf := make([]byte, 65535)
	consecutiveErrors := 0

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := t.tunDev.Read(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}

			consecutiveErrors++
			if consecutiveErrors > maxConsecutiveErrors {
				log.Printf("[tunnel] TUN 设备读取连续失败 %d 次，退出: %v", consecutiveErrors, err)
				return
			}

			time.Sleep(errorBackoff)
			continue
		}

		consecutiveErrors = 0

		if n < 20 {
			continue
		}

		// Validate IPv4 header (no copy needed — handlers copy data internally)
		if buf[0]>>4 != 4 {
			continue
		}
		ihl := int(buf[0]&0x0F) * 4
		if ihl < 20 || n < ihl {
			continue
		}

		srcIP := net.IP(buf[12:16])
		dstIP := net.IP(buf[16:20])
		t.routePacket(buf[:n], srcIP, dstIP)
	}
}
