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
	"github.com/holipay/gametunnel/internal/netutil"
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
const tunChanSize = 2048

// tunWorkers is the number of goroutines processing TUN packets.
// 4 workers allow encryption + marshal to overlap with TUN reads.
const tunWorkers = 4

// ctrlTimerPool reuses timers for sendCtrl to avoid per-call allocation.
// Timers are Reset before use and drained after use to ensure clean state.
var ctrlTimerPool = sync.Pool{
	New: func() interface{} { return time.NewTimer(time.Hour) },
}

// ipKey converts an IP address to a [16]byte map key.
// Delegates to netutil.IPKey for shared implementation.
func ipKey(ip net.IP) [16]byte {
	return netutil.IPKey(ip)
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

// tryRateLimitHolePunch checks and updates the hole-punch rate limiter.
// Returns true if the punch is allowed (not rate-limited).
func (p *Peer) tryRateLimitHolePunch(backoff time.Duration) bool {
	now := time.Now()
	lastPunch := p.lastPunchBack.Load()
	if lastPunch != nil && now.Sub(*lastPunch) < backoff {
		return false
	}
	p.lastPunchBack.Store(&now)
	return true
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
	disconnectOnce atomic.Pointer[sync.Once]
	sendErrors     atomic.Int64 // send failure counter
	cancelKicks    atomic.Bool  // true if server sent a fatal kick (wrong password, version mismatch)

	// Client-side send rate limiter (token bucket, per-server)
	sendLimiter *clientSendLimiter

	// Server liveness tracking — updated by handleServerData
	lastServerResponse atomic.Pointer[time.Time]

	// Server version from AssignIP response (0 = old server without version)
	serverVersion uint16

	// Cached hole punch packet — built once on Connect, reused by
	// startHolePunch, handleHolePunchReceived, and sendP2PKeepalives.
	cachedPunchPacket []byte

	// End-to-end encryption (nil when no password)
	encCipher *crypto.Cipher // client→server (relay send, DirClientToServer)
	decCipher *crypto.Cipher // server→client (relay receive, DirServerToClient)
	p2pCipher *crypto.Cipher // client↔client (P2P direct, DirClientToClient)

	// P2P enhancement: NAT type detection and port prediction
	natProbeResult *netutil.NATProbeResult // NAT type from probe (nil if not probed)
	portPredictor  *netutil.PortPredictor  // port prediction for hole punching

	// FEC: forward error correction for packet loss recovery
	fecEncoder *netutil.FECEncoder // generates parity packets
	fecDecoder *netutil.FECDecoder // recovers lost packets

	// LZ4: lightweight compression
	lz4Encoder *netutil.LZ4Encoder
	lz4Decoder *netutil.LZ4Decoder

	// TCP fallback transport (nil when using UDP)
	tcpTransport *netutil.TCPTransport // TCP connection for when UDP is blocked

	// TUN reuse state — persists across Connect() calls
	lastAssignedIP net.IP                             // virtual IP from last registration
	lastMTU        int                                // MTU from last connection
	newTUNFunc     func(TunConfig) (TunDevice, error) // cached factory
}

// sendChanSize is the buffer size for the UDP send channel.
// Sized to absorb bursts without blocking callers.
const sendChanSize = 8192

// ctrlChanSize is the buffer size for the control packet channel.
// Control packets (keepalive, pong, peer request) must never be dropped.
const ctrlChanSize = 256

// New creates a new Tunnel. Call Connect to start it.
func New(cfg *Config) *Tunnel {
	t := &Tunnel{
		username: cfg.PlayerName,
		roomID:   cfg.RoomID,
		roomPass: cfg.RoomPassword,
		peers:    make(map[[16]byte]*Peer),
		sendCh:   make(chan sendJob, sendChanSize),
		ctrlCh:   make(chan sendJob, ctrlChanSize),
		tunCh:    make(chan tunJob, tunChanSize),
		// Default: 50 Mbps client send limit, 512 KB burst
		sendLimiter: newClientSendLimiter(50*1024*1024/8, 512*1024),
		// FEC: 8 packets per group (12.5% overhead)
		fecEncoder: netutil.NewFECEncoder(0),
		fecDecoder: netutil.NewFECDecoder(0),
		// LZ4 compression
		lz4Encoder: netutil.NewLZ4Encoder(),
		lz4Decoder: netutil.NewLZ4Decoder(),
	}
	t.disconnectOnce.Store(&sync.Once{})
	return t
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

	// Reset disconnectOnce so Disconnect() can send leave packet on each attempt.
	t.disconnectOnce.Store(&sync.Once{})

	// Close old conn to release the file descriptor and unblock
	// the old receiveFromServer goroutine before creating a new one.
	if t.conn != nil {
		t.conn.Close()
	}
	// Bind to [::] (dual-stack IPv6 socket) so the client can send UDP
	// packets to both IPv4 and IPv6 peer addresses. An IPv4-only socket
	// (0.0.0.0) cannot send to IPv6 destinations, which breaks hole
	// punching when the server reports IPv6 peer addresses.
	bindAddr := &net.UDPAddr{IP: net.IPv6zero, Port: 0}
	conn, err := net.ListenUDP("udp", bindAddr)
	if err != nil {
		return fmt.Errorf("%s", i18n.Format(i18n.T().ErrBindUDP, err))
	}

	// Normalize serverAddr.IP to 16 bytes so that IP comparisons with
	// addresses received on the IPv6 socket (always 16 bytes) work
	// correctly. IPv4 addresses become IPv4-mapped IPv6 (::ffff:x.x.x.x).
	if ip16 := sAddr.IP.To16(); ip16 != nil {
		sAddr.IP = ip16
	}
	t.serverAddr = sAddr
	// Tune UDP socket buffers for high-throughput gaming.
	// Ignoring errors — non-Linux platforms may not support this.
	setClientSocketBuffers(conn)
	t.conn = conn

	if err := t.register(ctx); err != nil {
		// UDP registration failed — try TCP fallback
		log.Printf("[tunnel] UDP registration failed: %v, trying TCP fallback...", err)
		tcp, tcpErr := netutil.DialTCP(serverAddr, 5*time.Second)
		if tcpErr != nil {
			conn.Close()
			return fmt.Errorf("%s (TCP fallback also failed: %v)", i18n.Format(i18n.T().ErrRegisterFailed, err), tcpErr)
		}
		t.tcpTransport = tcp
		// TCP registration uses the same protocol over TCP
		if regErr := t.registerTCP(ctx); regErr != nil {
			tcp.Close()
			conn.Close()
			return fmt.Errorf("%s", i18n.Format(i18n.T().ErrRegisterFailed, regErr))
		}
		log.Printf("[tunnel] connected via TCP fallback")
	}

	// ── NAT type probe (background, non-blocking) ─────────────────
	if t.tcpTransport == nil { // only probe over UDP
		go func() {
			result, err := netutil.ProbeNATType(conn, sAddr)
			if err != nil {
				log.Printf("[nat-probe] probe failed: %v", err)
			} else {
				t.mu.Lock()
				t.natProbeResult = result
				t.portPredictor = netutil.PortPredictorFromNATProbe([]*netutil.NATProbeResult{result})
				t.mu.Unlock()
				log.Printf("[nat-probe] NAT type: %d, external: %s:%d, RTT: %v",
					result.Type, result.ExternalIP, result.ExternalPort, result.RTT)
			}
		}()
	}

	// ── TUN device: reuse or create ─────────────────────────────────
	if err := t.ensureTUN(mtu); err != nil {
		conn.Close()
		return err
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
		t.sendLoop(runCtx, conn)
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
		t.keepaliveLoop(runCtx, runCancel)
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

// ensureTUN reuses or creates the TUN device based on whether the IP changed.
func (t *Tunnel) ensureTUN(mtu int) error {
	ipChanged := t.lastAssignedIP != nil && !t.virtualIP.Equal(t.lastAssignedIP)
	tunAlive := t.tunDev != nil

	switch {
	case tunAlive && !ipChanged:
		log.Printf("%s", i18n.Format(i18n.T().LogReuseTUN, t.virtualIP))
		if rc, ok := t.tunDev.(RouteConfigurator); ok {
			rc.ReconfigureRoutes()
		}

	case tunAlive && ipChanged:
		log.Printf("%s", i18n.Format(i18n.T().LogIPChanged, t.lastAssignedIP, t.virtualIP))
		t.tunDev.Close()
		t.tunDev = nil
		return t.createTUN(mtu)

	case !tunAlive:
		return t.createTUN(mtu)
	}
	return nil
}

// createTUN creates a new TUN device using the cached factory and current
// virtual IP/subnet/serverIP. Called when TUN doesn't exist or IP changed.
func (t *Tunnel) createTUN(mtu int) error {
	if t.newTUNFunc == nil {
		return fmt.Errorf("TUN factory not set")
	}
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
	if once := t.disconnectOnce.Load(); once != nil {
		once.Do(func() {
			if t.serverAddr != nil {
				packet := protocol.EncodeChecked(protocol.TypeDisconnect, nil)
				// Use high-priority control channel to ensure the disconnect
				// packet is sent even under heavy load
				t.sendCtrl(packet, t.serverAddr)
				time.Sleep(50 * time.Millisecond)
			}
			if t.conn != nil {
				t.conn.Close()
			}
		})
	}
}

// CloseTUN closes the TUN device if open. Call this when exiting the program
// (not on every reconnect — the TUN should survive transient disconnections).
func (t *Tunnel) CloseTUN() {
	t.mu.Lock()
	dev := t.tunDev
	t.tunDev = nil
	t.lastAssignedIP = nil
	t.mu.Unlock()
	if dev != nil {
		dev.Close()
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

	// Server version from handshake (0 = old server without version)
	ServerVersion uint16

	// Connection quality metrics
	AvgRTT      float64 // average RTT in ms across all peers (0 = no data)
	LossRate    float64 // average loss rate 0.0-1.0
	P2PPeers    int     // number of peers with direct P2P connection
	RelayPeers  int     // number of peers using relay

	// P2P enhancement: NAT type info
	NATType     netutil.NATType // NAT type from probe (0 = unknown)
	ExternalIP  net.IP          // external IP as seen by server
	ExternalPort int            // external port as seen by server
}

// Status returns a snapshot of the current tunnel state.
func (t *Tunnel) Status() TunnelStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()

	st := TunnelStatus{
		Connected:     t.tunDev != nil && t.virtualIP != nil,
		VirtualIP:     t.virtualIP,
		SubnetMask:    t.subnetMask,
		ServerIP:      t.serverIP,
		PeerCount:     len(t.peers),
		ServerVersion: t.serverVersion,
	}

	// NAT probe info
	if t.natProbeResult != nil {
		st.NATType = t.natProbeResult.Type
		st.ExternalIP = t.natProbeResult.ExternalIP
		st.ExternalPort = t.natProbeResult.ExternalPort
	}

	if !st.Connected || len(t.peers) == 0 {
		return st
	}

	for _, p := range t.peers {
		if p.DirectReach.Load() {
			st.P2PPeers++
		} else {
			st.RelayPeers++
		}
	}

	return st
}

// sendLoop is the dedicated UDP send goroutine. It consumes from sendCh and
// writes to the UDP socket serially, eliminating mutex contention on the
// send path. Callers use sendUDP() which is non-blocking (channel send).
// Uses batch draining to reduce per-packet syscall overhead.
// conn is captured locally to avoid data races with Connect() reassigning t.conn.
func (t *Tunnel) sendLoop(ctx context.Context, conn *net.UDPConn) {
	const batchSize = 64
	var batch [batchSize]sendJob

	for {
		select {
		case <-ctx.Done():
			// Drain control packets first (disconnect, keepalive),
			// then data packets. Use a short timeout to avoid blocking.
			drainTimer := time.NewTimer(200 * time.Millisecond)
			for {
				select {
				case job := <-t.ctrlCh:
					t.writeUDP(conn, job.data, job.addr)
				case <-drainTimer.C:
					drainTimer.Stop()
					return
				default:
					goto drainDataOnly
				}
			}
		drainDataOnly:
			for {
				select {
				case job := <-t.sendCh:
					t.writeUDP(conn, job.data, job.addr)
				case job := <-t.ctrlCh:
					t.writeUDP(conn, job.data, job.addr)
				case <-drainTimer.C:
					drainTimer.Stop()
					return
				}
			}
		case job := <-t.ctrlCh:
			batch[0] = job
			n := t.drainBatch(batch[:], 1)
			for i := 0; i < n; i++ {
				t.writeUDP(conn, batch[i].data, batch[i].addr)
			}

		case job := <-t.sendCh:
			batch[0] = job
			n := t.drainBatch(batch[:], 1)
			for i := 0; i < n; i++ {
				t.writeUDP(conn, batch[i].data, batch[i].addr)
			}
		}
	}
}

// drainBatch fills batch[start:] from ctrlCh then sendCh, returning total count.
func (t *Tunnel) drainBatch(batch []sendJob, start int) int {
	n := start
	// Drain control packets (high priority)
	for n < cap(batch) {
		select {
		case batch[n] = <-t.ctrlCh:
			n++
		default:
			goto drainData
		}
	}
drainData:
	// Drain data packets
	for n < cap(batch) {
		select {
		case batch[n] = <-t.sendCh:
			n++
		default:
			return n
		}
	}
	return n
}

// writeUDP performs the actual UDP write. conn is passed explicitly to avoid
// data races with Connect() reassigning t.conn.
func (t *Tunnel) writeUDP(conn *net.UDPConn, data []byte, addr *net.UDPAddr) {
	if conn != nil {
		if _, err := conn.WriteToUDP(data, addr); err != nil {
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
	// Check client-side rate limit
	if !t.sendLimiter.allow(len(data)) {
		t.sendErrors.Add(1)
		return
	}

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
