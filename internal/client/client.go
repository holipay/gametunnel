// Package client implements the GameTunnel client (Windows).
//
// It creates a TUN virtual network device, registers with the server,
// and relays game traffic between the local machine and peers.
package client

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/holipay/gametunnel/internal/protocol"
)

// Peer represents a remote player.
type Peer struct {
	VirtualIP  net.IP
	PublicAddr *net.UDPAddr
	Username   string
}

// TunDevice abstracts the TUN device for testability and platform independence.
type TunDevice interface {
	Read(buf []byte) (int, error)
	Write(data []byte) (int, error)
	Close() error
}

// TunConfig holds the parameters needed to create a TUN device.
// Populated by Connect after successful registration.
type TunConfig struct {
	VirtualIP  net.IP
	SubnetMask net.IPMask
	ServerIP   net.IP
	MTU        int
}

// Tunnel is the GameTunnel client.
type Tunnel struct {
	conn       *net.UDPConn
	connMu     sync.Mutex // protects WriteToUDP
	serverAddr *net.UDPAddr
	tunDev     TunDevice
	virtualIP  net.IP
	serverIP   net.IP
	subnetMask net.IPMask
	peers      map[string]*Peer
	mu         sync.RWMutex
	username   string
	roomID     string
	roomPass   string
}

// New creates a new Tunnel. Call Connect to start it.
func New(cfg *Config) *Tunnel {
	return &Tunnel{
		username: cfg.PlayerName,
		roomID:   cfg.RoomID,
		roomPass: cfg.RoomPassword,
		peers:    make(map[string]*Peer),
	}
}

// Connect registers with the server, creates the TUN device via newTUN,
// and starts the relay loops. It blocks until ctx is cancelled.
// The newTUN callback receives TunConfig populated with the virtual IP,
// subnet mask, and server IP assigned during registration.
func (t *Tunnel) Connect(ctx context.Context, serverAddr string, mtu int, newTUN func(TunConfig) (TunDevice, error)) error {
	sAddr, err := net.ResolveUDPAddr("udp4", serverAddr)
	if err != nil {
		return fmt.Errorf("服务器地址无效: %w", err)
	}
	t.serverAddr = sAddr

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return fmt.Errorf("绑定 UDP 失败: %w", err)
	}
	t.conn = conn

	if err := t.register(ctx); err != nil {
		return fmt.Errorf("注册失败: %w", err)
	}

	tunCfg := TunConfig{
		VirtualIP:  t.virtualIP,
		SubnetMask: t.subnetMask,
		ServerIP:   t.serverIP,
		MTU:        mtu,
	}
	tunDev, err := newTUN(tunCfg)
	if err != nil {
		return fmt.Errorf("创建 TUN 失败: %w", err)
	}
	t.tunDev = tunDev
	defer tunDev.Close()

	go t.receiveFromServer(ctx)
	go t.receiveFromTUN(ctx)
	go t.keepaliveLoop(ctx)
	go t.peerDiscoveryLoop(ctx)

	<-ctx.Done()
	log.Printf("[tunnel] 断开连接")
	return nil
}

// Disconnect gracefully disconnects from the server.
func (t *Tunnel) Disconnect() {
	if t.conn != nil && t.serverAddr != nil {
		packet := protocol.EncodeChecked(protocol.TypeDisconnect, nil)
		t.sendUDP(packet, t.serverAddr)
		time.Sleep(50 * time.Millisecond)
		t.conn.Close()
	}
}

// VirtualIP returns the assigned virtual IP (valid after Connect).
func (t *Tunnel) VirtualIP() net.IP {
	return t.virtualIP
}

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

// startHolePunch sends NAT hole punch packets to a peer.
func (t *Tunnel) startHolePunch(peerIP net.IP) {
	t.mu.RLock()
	peer, ok := t.peers[peerIP.String()]
	t.mu.RUnlock()
	if !ok || peer.PublicAddr == nil {
		return
	}

	punchPayload := make([]byte, 4)
	copy(punchPayload, peerIP.To4())
	packet := protocol.EncodeChecked(protocol.TypeHolePunch, punchPayload)

	for i := 0; i < 5; i++ {
		t.sendUDP(packet, peer.PublicAddr)
		time.Sleep(200 * time.Millisecond)
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

// sendUDP is a thread-safe UDP write.
func (t *Tunnel) sendUDP(data []byte, addr *net.UDPAddr) {
	t.connMu.Lock()
	defer t.connMu.Unlock()
	if t.conn != nil {
		t.conn.WriteToUDP(data, addr)
	}
}

// keepaliveLoop sends periodic keepalive packets to the server.
func (t *Tunnel) keepaliveLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	packet := protocol.EncodeChecked(protocol.TypeKeepAlive, nil)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.sendUDP(packet, t.serverAddr)
		}
	}
}

// peerDiscoveryLoop periodically requests the peer list from the server.
func (t *Tunnel) peerDiscoveryLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	packet := protocol.EncodeChecked(protocol.TypePeerRequest, nil)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.sendUDP(packet, t.serverAddr)
		}
	}
}
