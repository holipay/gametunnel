package client

import (
	"context"
	"log"
	"net"

	"github.com/holipay/gametunnel/internal/protocol"
)

// receiveFromServer handles packets from the server.
func (t *Tunnel) receiveFromServer(ctx context.Context) {
	buf := make([]byte, 65535)
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
				continue
			}
		}

		pkt := make([]byte, n)
		copy(pkt, buf[:n])

		msg, err := protocol.DecodeChecked(pkt)
		if err != nil {
			continue
		}

		switch msg.Type {
		case protocol.TypePeerInfo:
			t.handlePeerInfo(msg.Payload)
		case protocol.TypeData:
			t.handleDataFromServer(msg.Payload)
		case protocol.TypeHolePunch:
			// NAT mapping established
		}
	}
}

// handlePeerInfo updates the peer list from the server.
func (t *Tunnel) handlePeerInfo(payload []byte) {
	info, err := protocol.UnmarshalPeerInfo(payload)
	if err != nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	newPeers := make(map[string]*Peer, len(info.Peers))
	for _, entry := range info.Peers {
		key := entry.VirtualIP.String()
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
			go t.startHolePunch(entry.VirtualIP)
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
		t.tunDev.Write(dp.Data)
	}
}

// receiveFromTUN reads IP packets from the TUN device and routes them.
func (t *Tunnel) receiveFromTUN(ctx context.Context) {
	buf := make([]byte, 65535)
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
				continue
			}
		}
		if n < 20 {
			continue
		}

		pkt := make([]byte, n)
		copy(pkt, buf[:n])

		// Validate IPv4 header
		if pkt[0]>>4 != 4 {
			continue
		}
		ihl := int(pkt[0]&0x0F) * 4
		if ihl < 20 || n < ihl {
			continue
		}

		srcIP := net.IP(pkt[12:16])
		dstIP := net.IP(pkt[16:20])
		t.routePacket(pkt, srcIP, dstIP)
	}
}
