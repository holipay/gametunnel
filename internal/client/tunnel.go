package client

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/nat"
	"github.com/holipay/gametunnel/internal/netutil"
	"github.com/holipay/gametunnel/internal/pool"
	"github.com/holipay/gametunnel/internal/protocol"
)

// sendJob is a single UDP send request, consumed by the dedicated sendLoop goroutine.
type sendJob struct {
	data []byte
	addr *net.UDPAddr
}

// tunJob is a TUN packet dispatched from the reader to worker goroutines.
// The packet data is a copy (not a slice of the shared TUN read buffer).
// srcIP/dstIP use [4]byte to avoid net.IP heap escape via channel send.
type tunJob struct {
	data  []byte
	srcIP [4]byte
	dstIP [4]byte
}

// tunChanSize is the buffer size for the TUN worker channel.
// Sized to absorb bursts from the TUN device without blocking the reader.
const tunChanSize = 2048

// tunWorkers is the number of goroutines processing TUN packets.
// 4 workers allow encryption + marshal to overlap with TUN reads.
const tunWorkers = 4

// Peer represents a remote player.
type Peer struct {
	VirtualIP     net.IP
	PublicAddr    atomic.Pointer[net.UDPAddr]
	Username      string
	NATType       nat.NATType              // peer's NAT type from PeerInfo (0 = unknown)
	DirectReach   atomic.Bool               // true if P2P direct path has been confirmed
	observedPorts []int                     // historical external ports for port prediction
	lastSeen      atomic.Int64 // last time server reported this peer (UnixNano)
	lastPunchBack atomic.Pointer[time.Time] // rate limit for hole punch responses
	stale         bool          // mark-and-sweep flag, only valid under t.mu in handlePeerInfo (not atomic)
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
// All TUN implementations must provide device info, route management, and
// basic I/O. This ensures consistent behavior across platforms.
type TunDevice interface {
	Read(buf []byte) (int, error)
	Write(data []byte) (int, error)
	Close() error
	Name() string
	MTU() int
	// ReconfigureRoutes re-applies routes without recreating the device.
	// Called on reconnect to fix routes that may have been modified by the OS.
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

// sendChanSize is the buffer size for the UDP send channel.
// Sized to absorb bursts without blocking callers.
const sendChanSize = 8192

// ctrlChanSize is the buffer size for the control packet channel.
// Control packets (keepalive, pong, peer request) must never be dropped.
const ctrlChanSize = 256

// New creates a new Tunnel. Call Connect to start it.
func New(cfg *Config) *Tunnel {
	t := &Tunnel{
		session: session{
			username: cfg.PlayerName,
			roomID:   cfg.RoomID,
			roomPass: cfg.RoomPassword,
		},
		peers:       make(map[[16]byte]*Peer),
		sendCh:      make(chan sendJob, sendChanSize),
		ctrlCh:      make(chan sendJob, ctrlChanSize),
		tunCh:       make(chan tunJob, tunChanSize),
		rebindAckCh: make(chan *protocol.RebindAckPayload, 1),
		// Default: 50 Mbps client send limit, 512 KB burst
		sendLimiter: newClientSendLimiter(50*1024*1024/8, 512*1024),
	}
	t.disconnectOnce.Store(&sync.Once{})
	return t
}

// Connect registers with the server, creates or recreates the TUN device,
// and starts the relay loops. It blocks until ctx is cancelled or a
// goroutine exits due to error (e.g. dead TUN device, lost server connection).
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

	t.stopPreviousRun()

	conn, err := t.dialServer(sAddr)
	if err != nil {
		return err
	}

	if err := t.registerWithFallback(ctx, serverAddr, conn); err != nil {
		return err
	}

	// Initialize probeDone channel for async NAT probe
	t.mu.Lock()
	t.nat.probeDone = make(chan struct{})
	t.mu.Unlock()
	t.probeNATAsync(conn, sAddr)

	if err := t.ensureTUN(mtu); err != nil {
		conn.Close()
		if t.tcpTransport != nil {
			t.tcpTransport.Close()
			t.tcpTransport = nil
		}
		return err
	}

	return t.startRelayLoops(ctx, conn, sAddr)
}

// stopPreviousRun cancels and waits for the previous Connect's goroutines.
func (t *Tunnel) stopPreviousRun() {
	if t.runCancel != nil {
		t.runCancel()
	}
	if t.runDone != nil {
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-t.runDone:
			timer.Stop()
		case <-timer.C:
			log.Printf("[tunnel] old goroutines did not exit within 5s, proceeding anyway")
		}
	}
}

// dialServer creates a dual-stack UDP connection and normalizes the server address.
func (t *Tunnel) dialServer(sAddr *net.UDPAddr) (*net.UDPConn, error) {
	if t.conn != nil {
		t.conn.Close()
	}

	bindAddr := &net.UDPAddr{IP: net.IPv6zero, Port: 0}
	conn, err := net.ListenUDP("udp", bindAddr)
	if err != nil {
		return nil, fmt.Errorf("%s", i18n.Format(i18n.T().ErrBindUDP, err))
	}

	if ip16 := sAddr.IP.To16(); ip16 != nil {
		sAddr.IP = ip16
	}
	t.serverAddr.Store(sAddr)
	if err := netutil.SetSocketBuffers(conn); err != nil {
		log.Printf("set socket buffers: %v", err)
	}
	t.conn = conn
	return conn, nil
}

// registerWithFallback tries UDP registration, falls back to TCP on failure.
func (t *Tunnel) registerWithFallback(ctx context.Context, serverAddr string, conn *net.UDPConn) error {
	if err := t.register(ctx, conn); err != nil {
		log.Printf("[tunnel] UDP registration failed: %v, trying TCP fallback...", err)
		tcp, tcpErr := netutil.DialTCP(serverAddr, 5*time.Second)
		if tcpErr != nil {
			conn.Close()
			return fmt.Errorf("%s (TCP fallback also failed: %v)", i18n.Format(i18n.T().ErrRegisterFailed, err), tcpErr)
		}
		t.tcpTransport = tcp
		if regErr := t.registerTCP(ctx); regErr != nil {
			tcp.Close()
			conn.Close()
			return fmt.Errorf("%s", i18n.Format(i18n.T().ErrRegisterFailed, regErr))
		}
		log.Printf("[tunnel] connected via TCP fallback")
	}
	return nil
}

// probeNAT performs NAT type detection asynchronously in a background goroutine.
// The result is stored in t.nat.probeResult when complete. Hole punch goroutines
// that need the result will wait on natProbeDone.
func (t *Tunnel) probeNATAsync(conn *net.UDPConn, sAddr *net.UDPAddr) {
	t.mu.RLock()
	alreadyProbed := t.nat.probeResult != nil
	t.mu.RUnlock()

	if t.tcpTransport != nil || alreadyProbed {
		close(t.nat.probeDone)
		return
	}

	go func() {
		defer close(t.nat.probeDone)

		result, err := nat.ProbeNATType(conn, sAddr)
		if err != nil {
			log.Printf("[nat-probe] probe failed: %v", err)
			return
		}

		t.mu.Lock()
		t.nat.probeResult = result
		t.nat.portPredictor = nat.PortPredictorFromNATProbe([]*nat.NATProbeResult{result})
		t.mu.Unlock()
		log.Printf("[nat-probe] NAT type: %d, external: %s:%d, RTT: %v",
			result.Type, result.ExternalIP, result.ExternalPort, result.RTT)
	}()
}

// startRelayLoops starts all relay goroutines and blocks until shutdown.
func (t *Tunnel) startRelayLoops(ctx context.Context, conn *net.UDPConn, sAddr *net.UDPAddr) error {
	runCtx, runCancel := context.WithCancel(ctx)
	t.runCancel = runCancel
	t.runDone = make(chan struct{})
	t.runWg = sync.WaitGroup{}

	var once sync.Once
	onGoroutineExit := func(name string) {
		once.Do(func() {
			log.Printf("%s", i18n.Format(i18n.T().LogPeerExit, name))
			runCancel()
		})
	}

	startGoroutine := func(name string, fn func()) {
		t.runWg.Add(1)
		go func() {
			defer t.runWg.Done()
			fn()
			onGoroutineExit(name)
		}()
	}

	startGoroutine("sendLoop", func() { t.sendLoop(runCtx, conn) })
	startGoroutine("receiveFromServer", func() { t.receiveFromServer(runCtx, conn, sAddr) })
	startGoroutine("receiveFromTUN", func() { t.receiveFromTUN(runCtx) })
	for i := 0; i < tunWorkers; i++ {
		startGoroutine("tunWorker", func() { t.tunWorker(runCtx) })
	}
	startGoroutine("keepaliveLoop", func() { t.keepaliveLoop(runCtx, runCancel) })
	startGoroutine("peerDiscoveryLoop", func() { t.peerDiscoveryLoop(runCtx) })
	startGoroutine("stalePeerCleanupLoop", func() { t.stalePeerCleanupLoop(runCtx) })
	startGoroutine("holePunchRetryLoop", func() { t.holePunchRetryLoop(runCtx) })
	startGoroutine("p2pKeepaliveLoop", func() { t.p2pKeepaliveLoop(runCtx) })

	<-runCtx.Done()

	go func() {
		t.runWg.Wait()
		close(t.runDone)
	}()

	timer := time.NewTimer(5 * time.Second)
	select {
	case <-t.runDone:
		timer.Stop()
	case <-timer.C:
		log.Printf("[tunnel] old goroutines did not exit within 5s, proceeding anyway")
	}

	log.Printf("%s", i18n.T().LogTunnelDisconnect)
	return nil
}

// ensureTUN reuses or creates the TUN device based on whether the IP changed.
func (t *Tunnel) ensureTUN(mtu int) error {
	ipChanged := t.lastAssignedIP != nil && !t.session.virtualIP.Equal(t.lastAssignedIP)
	tunAlive := t.tunDev.Load() != nil

	switch {
	case tunAlive && !ipChanged:
		log.Printf("%s", i18n.Format(i18n.T().LogRecreateTUN, t.session.virtualIP))
		if v := t.tunDev.Load(); v != nil {
			v.(TunDevice).Close()
		}
		return t.createTUN(mtu)

	case tunAlive && ipChanged:
		log.Printf("%s", i18n.Format(i18n.T().LogIPChanged, t.lastAssignedIP, t.session.virtualIP))
		t.tunDev.Load().(TunDevice).Close()
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
		VirtualIP:  t.session.virtualIP,
		SubnetMask: t.session.subnetMask,
		ServerIP:   t.session.serverIP,
		MTU:        mtu,
	}
	dev, err := t.newTUNFunc(tunCfg)
	if err != nil {
		return fmt.Errorf("%s", i18n.Format(i18n.T().ErrCreateTUN, err))
	}
	t.tunDev.Store(dev)
	t.lastAssignedIP = append(net.IP(nil), t.session.virtualIP...) // defensive copy
	t.lastMTU = mtu
	return nil
}

// Disconnect gracefully disconnects from the server.
// Safe to call multiple times (uses sync.Once).
func (t *Tunnel) Disconnect() {
	if once := t.disconnectOnce.Load(); once != nil {
		once.Do(func() {
			if addr := t.serverAddr.Load(); addr != nil {
				packet := protocol.EncodeChecked(protocol.TypeDisconnect, nil)
				// Use high-priority control channel to ensure the disconnect
				// packet is sent even under heavy load
				t.sendCtrl(packet, addr)
				time.Sleep(50 * time.Millisecond)
			}
			// Cancel context to signal hole punch goroutines to exit
			if t.runCancel != nil {
				t.runCancel()
			}
			// Wait for outstanding hole punch goroutines to finish
			t.holePunchWg.Wait()
			t.mu.Lock()
			c := t.conn
			tcp := t.tcpTransport
			t.conn = nil
			t.tcpTransport = nil
			t.mu.Unlock()
			if c != nil {
				c.Close()
			}
			if tcp != nil {
				tcp.Close()
			}
		})
	}
}

// CloseTUN closes the TUN device if open. Call this when exiting the program
// (not on every reconnect — the TUN is recreated by ensureTUN).
func (t *Tunnel) CloseTUN() {
	t.closeTUNOnce.Do(func() {
		t.mu.Lock()
		dev, _ := t.tunDev.Load().(TunDevice)
		t.lastAssignedIP = nil
		t.mu.Unlock()
		if dev != nil {
			dev.Close()
		}
	})
}

// VirtualIP returns the assigned virtual IP (valid after Connect).
func (t *Tunnel) VirtualIP() net.IP {
	return t.session.virtualIP
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
	NATType     nat.NATType // NAT type from probe (0 = unknown)
	ExternalIP  net.IP          // external IP as seen by server
	ExternalPort int            // external port as seen by server
}

// Status returns a snapshot of the current tunnel state.
func (t *Tunnel) Status() TunnelStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()

	st := TunnelStatus{
		Connected:     t.tunDev.Load() != nil && t.session.virtualIP != nil,
		VirtualIP:     t.session.virtualIP,
		SubnetMask:    t.session.subnetMask,
		ServerIP:      t.session.serverIP,
		PeerCount:     len(t.peers),
		ServerVersion: uint16(t.session.serverVersion.Load()),
	}

	// NAT probe info
	if t.nat.probeResult != nil {
		st.NATType = t.nat.probeResult.Type
		st.ExternalIP = t.nat.probeResult.ExternalIP
		st.ExternalPort = t.nat.probeResult.ExternalPort
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
	// Reserve rate limiter tokens first, refund if channel send fails.
	// This prevents token starvation when the channel is full.
	res := t.sendLimiter.tryReserve(len(data))
	if !res.ok() {
		t.sendErrors.Add(1)
		return
	}

	select {
	case t.sendCh <- sendJob{data: data, addr: addr}:
		res.commit()
	default:
		res.cancel()
		n := t.sendErrors.Add(1)
		if n == 1 || n%100 == 0 {
			log.Printf("%s", i18n.Format(i18n.T().LogSendFail, n, fmt.Errorf("send channel full")))
		}
	}
}

// sendCtrl enqueues a control packet (keepalive, pong, peer request, hole punch).
// Uses a non-blocking send to avoid cascading delays when multiple goroutines
// contend for the ctrl channel. Under burst load, drops are preferred over
// blocking caller goroutines (which would delay keepalive timers, etc.).
func (t *Tunnel) sendCtrl(data []byte, addr *net.UDPAddr) {
	select {
	case t.ctrlCh <- sendJob{data: data, addr: addr}:
	default:
		n := t.sendErrors.Add(1)
		if n == 1 || n%100 == 0 {
			log.Printf("%s", i18n.Format(i18n.T().LogSendFail, n, fmt.Errorf("ctrl channel full")))
		}
	}
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
			pool.PktBufPut(job.data)
		}
	}
}
