package client

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/holipay/gametunnel/internal/protocol"
	"github.com/holipay/gametunnel/internal/crypto"
	"github.com/holipay/gametunnel/internal/i18n"
)

// sendJob is a single UDP send request, consumed by the dedicated sendLoop goroutine.
type sendJob struct {
	data []byte
	addr *net.UDPAddr
}

// tunJob is a TUN packet dispatched from the reader to worker goroutines.
// The packet data is a copy (not a slice of the shared TUN read buffer).
type tunJob struct {
	data  []byte
	srcIP net.IP
	dstIP net.IP
}

// tunChanSize is the buffer size for the TUN worker channel.
// Sized to absorb bursts from the TUN device without blocking the reader.
const tunChanSize = 512

// tunWorkers is the number of goroutines processing TUN packets.
// 2 workers allow encryption + marshal to overlap with TUN reads.
const tunWorkers = 2

// ctrlTimerPool reuses timers for sendCtrl to avoid per-call allocation.
// Timers are Reset before use and drained after use to ensure clean state.
var ctrlTimerPool = sync.Pool{
	New: func() interface{} { return time.NewTimer(0) },
}

// ipKey converts an IP address to a [16]byte map key.
// IPv4 addresses are automatically mapped to v4-in-v6 format (::ffff:x.x.x.x).
func ipKey(ip net.IP) [16]byte {
	var k [16]byte
	copy(k[:], ip.To16())
	return k
}

// isLoopback reports whether ip is a loopback address (127.0.0.0/8 or ::1).
func isLoopback(ip net.IP) bool {
	return ip.IsLoopback()
}

// Peer represents a remote player.
type Peer struct {
	VirtualIP     net.IP
	PublicAddr    *net.UDPAddr
	Username      string
	DirectReach   atomic.Bool               // true if P2P direct path has been confirmed
	lastSeen      atomic.Pointer[time.Time] // last time server reported this peer
	lastPunchBack atomic.Pointer[time.Time] // rate limit for hole punch responses
}

// TunDevice abstracts the TUN device for testability and platform independence.
type TunDevice interface {
	Read(buf []byte) (int, error)
	Write(data []byte) (int, error)
	Close() error
}

// RouteConfigurator is an optional interface for TUN devices that support
// reconfiguring routes without recreation. Used on reconnect to re-apply
// routes that may have been modified by the OS during disconnection.
type RouteConfigurator interface {
	ReconfigureRoutes()
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
	conn           *net.UDPConn
	sendCh         chan sendJob // dedicated channel for UDP sends (replaces connMu)
	ctrlCh         chan sendJob // high-priority channel for control packets (never dropped)
	tunCh          chan tunJob  // TUN packet channel for worker pool
	serverAddr     *net.UDPAddr
	tunDev         TunDevice
	virtualIP      net.IP
	serverIP       net.IP
	serverIPKey     [16]byte // cached serverIP as [16]byte for fast comparison
	subnetMask     net.IPMask
	cachedSubnet   *net.IPNet // cached subnet for broadcast detection
	peers          map[[16]byte]*Peer
	mu             sync.RWMutex
	username       string
	roomID         string
	roomPass       string
	disconnectOnce sync.Once
	sendErrors     atomic.Int64 // send failure counter

	// Server liveness tracking — updated by handleServerData
	lastServerResponse atomic.Pointer[time.Time]

	// Cached hole punch packet — built once on Connect, reused by
	// startHolePunch, handleHolePunchReceived, and sendP2PKeepalives.
	cachedPunchPacket []byte

	// End-to-end encryption (nil when no password)
	encCipher *crypto.Cipher // client→server (relay send, DirClientToServer)
	decCipher *crypto.Cipher // server→client (relay receive, DirServerToClient)
	p2pCipher *crypto.Cipher // client↔client (P2P direct, DirClientToClient)

	// TUN reuse state — persists across Connect() calls
	lastAssignedIP net.IP                             // virtual IP from last registration
	lastMTU        int                                // MTU from last connection
	newTUNFunc     func(TunConfig) (TunDevice, error) // cached factory
}

// sendChanSize is the buffer size for the UDP send channel.
// Sized to absorb bursts without blocking callers.
const sendChanSize = 4096

// ctrlChanSize is the buffer size for the control packet channel.
// Control packets (keepalive, pong, peer request) must never be dropped.
const ctrlChanSize = 256

// New creates a new Tunnel. Call Connect to start it.
func New(cfg *Config) *Tunnel {
	return &Tunnel{
		username: cfg.PlayerName,
		roomID:   cfg.RoomID,
		roomPass: cfg.RoomPassword,
		peers:    make(map[[16]byte]*Peer),
		sendCh:   make(chan sendJob, sendChanSize),
		ctrlCh:   make(chan sendJob, ctrlChanSize),
		tunCh:    make(chan tunJob, tunChanSize),
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

	sAddr, err := net.ResolveUDPAddr("udp", serverAddr)
	if err != nil {
		return fmt.Errorf("%s", i18n.Format(i18n.T().ErrInvalidServer, err))
	}
	t.serverAddr = sAddr

	// Reset disconnectOnce so Disconnect() can send leave packet on each attempt.
	t.disconnectOnce = sync.Once{}

	conn, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		return fmt.Errorf("%s", i18n.Format(i18n.T().ErrBindUDP, err))
	}
	t.conn = conn

	if err := t.register(ctx); err != nil {
		conn.Close()
		return fmt.Errorf("%s", i18n.Format(i18n.T().ErrRegisterFailed, err))
	}

	// ── TUN device: reuse or create ─────────────────────────────────
	ipChanged := t.lastAssignedIP != nil && !t.virtualIP.Equal(t.lastAssignedIP)
	tunAlive := t.tunDev != nil

	switch {
	case tunAlive && !ipChanged:
		log.Printf("%s", i18n.Format(i18n.T().LogReuseTUN, t.virtualIP))
		// Re-apply routes that may have been modified by the OS during disconnection
		if rc, ok := t.tunDev.(RouteConfigurator); ok {
			rc.ReconfigureRoutes()
		}

	case tunAlive && ipChanged:
		log.Printf("%s", i18n.Format(i18n.T().LogIPChanged, t.lastAssignedIP, t.virtualIP))
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
			log.Printf("%s", i18n.Format(i18n.T().LogPeerExit, name))
			runCancel()
		})
	}

	go func() {
		t.sendLoop(runCtx)
		onGoroutineExit("sendLoop")
	}()
	go func() {
		t.receiveFromServer(runCtx)
		onGoroutineExit("receiveFromServer")
	}()
	go func() {
		t.receiveFromTUN(runCtx)
		onGoroutineExit("receiveFromTUN")
	}()
	for i := 0; i < tunWorkers; i++ {
		go func() {
			t.tunWorker(runCtx)
			onGoroutineExit("tunWorker")
		}()
	}
	go func() {
		t.keepaliveLoop(runCtx)
		onGoroutineExit("keepaliveLoop")
	}()
	go func() {
		t.peerDiscoveryLoop(runCtx)
		onGoroutineExit("peerDiscoveryLoop")
	}()
	go func() {
		t.stalePeerCleanupLoop(runCtx)
		onGoroutineExit("stalePeerCleanupLoop")
	}()
	go func() {
		t.holePunchRetryLoop(runCtx)
		onGoroutineExit("holePunchRetryLoop")
	}()
	go func() {
		t.p2pKeepaliveLoop(runCtx)
		onGoroutineExit("p2pKeepaliveLoop")
	}()

	<-runCtx.Done()

	log.Printf("%s", i18n.T().LogTunnelDisconnect)
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
		return fmt.Errorf("%s", i18n.Format(i18n.T().ErrCreateTUN, err))
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

// sendLoop is the dedicated UDP send goroutine. It consumes from sendCh and
// writes to the UDP socket serially, eliminating mutex contention on the
// send path. Callers use sendUDP() which is non-blocking (channel send).
func (t *Tunnel) sendLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			// Drain remaining sends with a short timeout to ensure
			// disconnect packets and final control messages are sent.
			drainTimer := time.NewTimer(200 * time.Millisecond)
			for {
				select {
				case job := <-t.ctrlCh:
					t.writeUDP(job.data, job.addr)
				case job := <-t.sendCh:
					t.writeUDP(job.data, job.addr)
				case <-drainTimer.C:
					drainTimer.Stop()
					return
				}
			}
		case job := <-t.ctrlCh:
			// Control packets (keepalive, pong, peer request) — always drain first
			t.writeUDP(job.data, job.addr)
		case job := <-t.sendCh:
			// Data packets — only process when no control packets pending
			select {
			case ctrlJob := <-t.ctrlCh:
				t.writeUDP(ctrlJob.data, ctrlJob.addr)
				t.writeUDP(job.data, job.addr) // process the data packet too
			default:
				t.writeUDP(job.data, job.addr)
			}
		}
	}
}

// writeUDP performs the actual UDP write. Only called from sendLoop.
func (t *Tunnel) writeUDP(data []byte, addr *net.UDPAddr) {
	if t.conn != nil {
		if _, err := t.conn.WriteToUDP(data, addr); err != nil {
			n := t.sendErrors.Add(1)
			if n == 1 || n%100 == 0 {
				log.Printf("%s", i18n.Format(i18n.T().LogSendFail, n, err))
			}
		}
	}
}

// sendUDP enqueues a UDP send via the send channel (non-blocking).
// Replaces the previous mutex-based approach to eliminate lock contention
// between the TUN reader, server reader, and keepalive goroutines.
func (t *Tunnel) sendUDP(data []byte, addr *net.UDPAddr) {
	select {
	case t.sendCh <- sendJob{data: data, addr: addr}:
	default:
		// Channel full — drop packet (backpressure)
		n := t.sendErrors.Add(1)
		if n == 1 || n%100 == 0 {
			log.Printf("%s", i18n.Format(i18n.T().LogSendFail, n, fmt.Errorf("send channel full")))
		}
	}
}

// sendCtrl enqueues a control packet (keepalive, pong, peer request, hole punch).
// Control packets use a separate high-priority channel with a short blocking window
// to avoid dropping critical keepalive packets under burst load.
func (t *Tunnel) sendCtrl(data []byte, addr *net.UDPAddr) {
	timer := ctrlTimerPool.Get().(*time.Timer)
	timer.Reset(50 * time.Millisecond)
	select {
	case t.ctrlCh <- sendJob{data: data, addr: addr}:
	case <-timer.C:
		// Channel full after 50ms — drop to avoid blocking caller indefinitely
		n := t.sendErrors.Add(1)
		if n == 1 || n%100 == 0 {
			log.Printf("%s", i18n.Format(i18n.T().LogSendFail, n, fmt.Errorf("ctrl channel full")))
		}
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	ctrlTimerPool.Put(timer)
}

// tunWorker processes TUN packets from the tunCh channel.
// Multiple workers run in parallel to overlap encryption + marshal with TUN reads.
func (t *Tunnel) tunWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-t.tunCh:
			t.routePacket(job.data, job.srcIP, job.dstIP)
		}
	}
}
