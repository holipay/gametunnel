package client

import (
	"context"
	"log"
	"net"
	"time"

	"github.com/holipay/gametunnel-protocol/protocol"
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

		// Distinguish server-relayed vs direct P2P by source address.
		// Direct peer packets arrive from the peer's public address;
		// server-relayed packets arrive from the server's address.
		if from != nil && t.serverAddr != nil && !from.IP.Equal(t.serverAddr.IP) {
			// Direct P2P packet from a peer's public address
			t.handleDirectData(from, msg)
		} else {
			// Server-relayed packet
			t.handleServerData(ctx, msg)
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
		t.sendUDP(protocol.EncodeChecked(protocol.TypePong, msg.Payload), t.serverAddr)
	case protocol.TypeHolePunch:
		t.handleHolePunchReceived(msg.Payload)
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

	dp, err := protocol.UnmarshalData(msg.Payload)
	if err != nil || len(dp.Data) == 0 || t.tunDev == nil {
		return
	}

	// Validate srcIP is a known peer (anti-spoofing)
	srcKey := ip4Key(dp.SrcIP)
	t.mu.RLock()
	peer, known := t.peers[srcKey]
	t.mu.RUnlock()
	if !known {
		return
	}

	// Verify the packet actually came from this peer's public address (IP + port)
	if peer.PublicAddr == nil || !from.IP.Equal(peer.PublicAddr.IP) || from.Port != peer.PublicAddr.Port {
		return
	}

	// Mark P2P direct path confirmed — this is the legitimate DirectReach signal
	peer.DirectReach.Store(true)

	if _, err := t.tunDev.Write(dp.Data); err != nil {
		log.Printf("[tunnel] TUN 写入失败: %v", err)
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

	newPeers := make(map[[4]byte]*Peer, len(info.Peers))
	for _, entry := range info.Peers {
		// Skip self — server sends full list including this client
		if entry.VirtualIP.Equal(t.virtualIP) {
			continue
		}
		key := ip4Key(entry.VirtualIP)
		if existing, ok := t.peers[key]; ok {
			// Check if peer's public address changed (NAT rebinding)
			addrChanged := existing.PublicAddr != nil && entry.PublicAddr != nil &&
				(!existing.PublicAddr.IP.Equal(entry.PublicAddr.IP) || existing.PublicAddr.Port != entry.PublicAddr.Port)
			if addrChanged {
				log.Printf("[tunnel] 玩家地址变更: %s (%s) %s → %s", entry.Username, entry.VirtualIP, existing.PublicAddr, entry.PublicAddr)
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
			log.Printf("[tunnel] 新玩家: %s (%s)", entry.Username, entry.VirtualIP)
			newPeerIPs = append(newPeerIPs, entry.VirtualIP)
		}
	}
	// Log removed peers
	for key, peer := range t.peers {
		if _, ok := newPeers[key]; !ok {
			log.Printf("[tunnel] 玩家离开: %s (%s)", peer.Username, peer.VirtualIP)
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
	dp, err := protocol.UnmarshalData(payload)
	if err != nil {
		return
	}
	if len(dp.Data) == 0 || t.tunDev == nil {
		return
	}

	srcKey := ip4Key(dp.SrcIP)

	// Allow traffic from the server's virtual IP (relay path) or known peers.
	if srcKey != t.serverIP4 {
		t.mu.RLock()
		_, known := t.peers[srcKey]
		t.mu.RUnlock()
		if !known {
			// Unknown srcIP — drop to prevent injection.
			return
		}
	}

	if _, err := t.tunDev.Write(dp.Data); err != nil {
		log.Printf("[tunnel] TUN 写入失败: %v", err)
	}
}

// receiveFromTUN reads IP packets from the TUN device and routes them.
func (t *Tunnel) receiveFromTUN(ctx context.Context) {
	buf := make([]byte, readBufSize)
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
		// Copy packet data before passing to routePacket.
		// The buffer is reused on the next Read, and sendUDP may not
		// complete before the next iteration overwrites it.
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		t.routePacket(pkt, srcIP, dstIP)
	}
}
