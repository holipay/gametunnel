package client

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/holipay/gametunnel-protocol/protocol"
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
	DirectReach atomic.Bool // true if P2P direct path has been confirmed
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
	conn          *net.UDPConn
	connMu        sync.Mutex // protects WriteToUDP
	serverAddr    *net.UDPAddr
	tunDev        TunDevice
	virtualIP     net.IP
	serverIP      net.IP
	serverIP4     [4]byte    // cached serverIP as [4]byte for fast comparison
	subnetMask    net.IPMask
	cachedSubnet  *net.IPNet // cached subnet for broadcast detection
	peers         map[[4]byte]*Peer
	mu            sync.RWMutex
	username      string
	roomID        string
	roomPass      string
	disconnectOnce sync.Once
	sendErrors     atomic.Int64 // send failure counter

	// TUN reuse state — persists across Connect() calls
	lastAssignedIP net.IP               // virtual IP from last registration
	lastMTU        int                  // MTU from last connection
	newTUNFunc     func(TunConfig) (TunDevice, error) // cached factory
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

// Connect registers with the server, creates or reuses the TUN device,
// and starts the relay loops. It blocks until ctx is cancelled or a
// goroutine exits due to error (e.g. dead TUN device, lost server connection).
//
// On subsequent calls (reconnect), if the server assigns the same virtual IP
// and the TUN device is still functional, it is reused without recreation.
// This avoids disrupting the game's network interface during transient
// server disconnections.
//
// The newTUN callback is only invoked when a new TUN device is actually needed.
// It is cached internally for potential reuse across reconnects.
func (t *Tunnel) Connect(ctx context.Context, serverAddr string, mtu int, newTUN func(TunConfig) (TunDevice, error)) error {
	// Cache the TUN factory for potential future reconnects.
	if newTUN != nil {
		t.newTUNFunc = newTUN
	}

	sAddr, err := net.ResolveUDPAddr("udp4", serverAddr)
	if err != nil {
		return fmt.Errorf("服务器地址无效: %w", err)
	}
	t.serverAddr = sAddr

	// Reset disconnectOnce so Disconnect() can send leave packet on each attempt.
	t.disconnectOnce = sync.Once{}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return fmt.Errorf("绑定 UDP 失败: %w", err)
	}
	t.conn = conn

	if err := t.register(ctx); err != nil {
		conn.Close()
		return fmt.Errorf("注册失败: %w", err)
	}

	// ── TUN device: reuse or create ─────────────────────────────────
	ipChanged := t.lastAssignedIP != nil && !t.virtualIP.Equal(t.lastAssignedIP)
	tunAlive := t.tunDev != nil

	switch {
	case tunAlive && !ipChanged:
		// Best case: TUN is alive and IP didn't change — reuse as-is.
		log.Printf("[tunnel] 复用 TUN 设备 (IP %s 未变)", t.virtualIP)

	case tunAlive && ipChanged:
		// IP changed — must recreate TUN with new IP/routes.
		log.Printf("[tunnel] IP 变更 %s → %s，重建 TUN 设备", t.lastAssignedIP, t.virtualIP)
		t.tunDev.Close()
		t.tunDev = nil
		if err := t.createTUN(mtu); err != nil {
			return err
		}

	case !tunAlive:
		// First connection or TUN was lost — create new.
		if err := t.createTUN(mtu); err != nil {
			return err
		}
	}

	// ── Start relay goroutines ──────────────────────────────────────
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	var once sync.Once
	onGoroutineExit := func(name string) {
		once.Do(func() {
			log.Printf("[tunnel] %s 退出，断开连接", name)
			runCancel()
		})
	}

	go func() {
		t.receiveFromServer(runCtx)
		onGoroutineExit("receiveFromServer")
	}()
	go func() {
		t.receiveFromTUN(runCtx)
		onGoroutineExit("receiveFromTUN")
	}()
	go func() {
		t.keepaliveLoop(runCtx)
		onGoroutineExit("keepaliveLoop")
	}()
	go func() {
		t.peerDiscoveryLoop(runCtx)
		onGoroutineExit("peerDiscoveryLoop")
	}()

	<-runCtx.Done()

	log.Printf("[tunnel] 断开连接")
	return nil
}

// createTUN creates a new TUN device using the cached factory and current
// virtual IP/subnet/serverIP. Called when TUN doesn't exist or IP changed.
func (t *Tunnel) createTUN(mtu int) error {
	tunCfg := TunConfig{
		VirtualIP:  t.virtualIP,
		SubnetMask: t.subnetMask,
		ServerIP:   t.serverIP,
		MTU:        mtu,
	}
	dev, err := t.newTUNFunc(tunCfg)
	if err != nil {
		return fmt.Errorf("创建 TUN 失败: %w", err)
	}
	t.tunDev = dev
	t.lastAssignedIP = append(net.IP(nil), t.virtualIP...) // defensive copy
	t.lastMTU = mtu
	return nil
}

// Disconnect gracefully disconnects from the server.
// Safe to call multiple times (uses sync.Once).
func (t *Tunnel) Disconnect() {
	t.disconnectOnce.Do(func() {
		if t.serverAddr != nil {
			packet := protocol.EncodeChecked(protocol.TypeDisconnect, nil)
			t.sendUDP(packet, t.serverAddr)
			time.Sleep(50 * time.Millisecond)
		}
		if t.conn != nil {
			t.conn.Close()
		}
	})
}

// CloseTUN closes the TUN device if open. Call this when exiting the program
// (not on every reconnect — the TUN should survive transient disconnections).
func (t *Tunnel) CloseTUN() {
	if t.tunDev != nil {
		t.tunDev.Close()
		t.tunDev = nil
		t.lastAssignedIP = nil
	}
}

// VirtualIP returns the assigned virtual IP (valid after Connect).
func (t *Tunnel) VirtualIP() net.IP {
	return t.virtualIP
}

// TunnelStatus is a point-in-time snapshot of the tunnel state.
type TunnelStatus struct {
	Connected  bool
	VirtualIP  net.IP
	SubnetMask net.IPMask
	ServerIP   net.IP
	PeerCount  int
}

// Status returns a snapshot of the current tunnel state.
func (t *Tunnel) Status() TunnelStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return TunnelStatus{
		Connected:  t.tunDev != nil && t.virtualIP != nil,
		VirtualIP:  t.virtualIP,
		SubnetMask: t.subnetMask,
		ServerIP:   t.serverIP,
		PeerCount:  len(t.peers),
	}
}

// sendUDP is a thread-safe UDP write.
func (t *Tunnel) sendUDP(data []byte, addr *net.UDPAddr) {
	t.connMu.Lock()
	defer t.connMu.Unlock()
	if t.conn != nil {
		if _, err := t.conn.WriteToUDP(data, addr); err != nil {
			if t.sendErrors.Add(1) == 1 {
				log.Printf("[tunnel] 发送失败: %v", err)
			}
		}
	}
}
