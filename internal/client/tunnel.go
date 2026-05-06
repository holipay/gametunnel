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

// ip4Key converts a 4-byte IPv4 address to a [4]byte map key.
// Panics if ip is not a valid IPv4 address.
func ip4Key(ip net.IP) [4]byte {
	ip4 := ip.To4()
	return [4]byte{ip4[0], ip4[1], ip4[2], ip4[3]}
}

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
	conn         *net.UDPConn
	connMu       sync.Mutex // protects WriteToUDP
	serverAddr   *net.UDPAddr
	tunDev       TunDevice
	virtualIP    net.IP
	serverIP     net.IP
	serverIP4    [4]byte    // cached serverIP as [4]byte for fast comparison
	subnetMask   net.IPMask
	cachedSubnet *net.IPNet // cached subnet for broadcast detection
	peers        map[[4]byte]*Peer
	mu           sync.RWMutex
	username     string
	roomID       string
	roomPass     string
}

// New creates a new Tunnel. Call Connect to start it.
func New(cfg *Config) *Tunnel {
	return &Tunnel{
		username: cfg.PlayerName,
		roomID:   cfg.RoomID,
		roomPass: cfg.RoomPassword,
		peers:    make(map[[4]byte]*Peer),
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

// sendUDP is a thread-safe UDP write.
func (t *Tunnel) sendUDP(data []byte, addr *net.UDPAddr) {
	t.connMu.Lock()
	defer t.connMu.Unlock()
	if t.conn != nil {
		t.conn.WriteToUDP(data, addr)
	}
}
