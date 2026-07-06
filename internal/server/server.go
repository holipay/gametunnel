// Package server implements the GameTunnel relay server.
//
// It accepts client registrations (with optional HMAC auth), relays game
// traffic between peers, and handles UDP broadcast forwarding for LAN
// game discovery.
package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/holipay/gametunnel/internal/netutil"
	"github.com/holipay/gametunnel/internal/pool"
	"github.com/holipay/gametunnel/internal/netkey"
	"github.com/holipay/gametunnel/internal/protocol"
	"github.com/holipay/gametunnel/internal/i18n"
)


// ── Server ─────────────────────────────────────────────────────

// Server is the GameTunnel relay server.
type Server struct {
	conn       *net.UDPConn
	sendQueue  *rateLimitedQueue // priority send queue with bandwidth limiting
	statusAddr  string   // HTTP status address, empty = disabled
	statusToken string   // status page access token, empty = no auth
	version     string
	lang       i18n.Lang
	startTime  time.Time
	verbose    bool

	closeMu    sync.Mutex
	ctx        context.Context // stored for use in packet handlers
	cancelCtx  context.CancelFunc // cancels ctx on Close()
	runWg      sync.WaitGroup // tracks the main read loop goroutine

	// Worker pool
	workers int
	pktCh   chan pktJob

	// Rate limiting: per-client packet count per window (sharded for low contention)
	rateShards *rateShardsArray
	rateTick   *time.Ticker

	// Time-series metrics
	metricsTS    *MetricsTimeSeries

	// Multi-room support
	multiRoom   bool
	rooms       map[string]*Room    // roomID → Room
	addrToRoom  map[netkey.RateKey]*Room   // client addr → Room (fast routing)
	roomMu      sync.RWMutex        // protects rooms + addrToRoom
	maxRooms    int                 // max auto-created rooms (0 = unlimited)

	// Default room (single-room mode)
	defaultRoom *Room

	// Base subnet for multi-room mode (stored separately since defaultRoom is nil)
	baseSubnet *net.IPNet

	// Room password (propagated to auto-created rooms in multi-room mode)
	roomPass string

	// Cached encrypted flag (true if any room uses password-based AEAD).
	// Computed once in New(); avoids re-evaluating per packet in handlePacket.
	cachedEncrypted bool

	// Bandwidth limiting
	bwLimiter    *BandwidthLimiter // per-client outbound bandwidth limiter

	// Cached local subnets (populated once at startup)
	localSubnets []*net.IPNet

	// Diagnostics
	sendErrors atomic.Int64 // send failure counter

	// State persistence
	stateDir      string       // directory for state file, empty = disabled
	stateLoadedAt time.Time    // when state was last loaded from disk
	persistDirty  atomic.Bool  // true if state needs to be written to disk

	// Operational metrics (lifetime counters, never reset)
	totalPacketsDropped atomic.Uint64 // packets dropped (rate limit, full channel, invalid)

	// TCP fallback listener for clients behind strict firewalls
	tcpListener *netutil.TCPListener // nil when TCP is disabled

	// pprof listener for runtime profiling (nil when disabled)
	pprofListener net.Listener

	// TCP bridge routing
	tcpPortCounter atomic.Uint32    // unique port assignment for TCP clients
	tcpBridges     sync.Map         // rateKey → *UDPTCPBridge

	// Server readiness signal
	ready chan struct{} // closed when Run() enters the main read loop
}

// pktJob represents a packet to be processed by the worker pool.
type pktJob struct {
	data []byte
	addr *net.UDPAddr
}

// Config holds server configuration.
type Config struct {
	Addr       string
	Subnet     *net.IPNet
	MaxPlayers int
	RoomPass   string
	StatusAddr  string // HTTP status address (e.g. ":4701"), empty = disabled
	StatusToken string // status page access token, empty = no auth
	Version    string
	Lang       i18n.Lang
	MaxPerIP       int    // max connections per IP (0 = use default 3)
	StateDir       string // directory for state persistence, empty = disabled
	MultiRoom      bool   // enable multi-room mode
	BandwidthLimit int    // per-client outbound bandwidth limit in bytes/sec (0 = default 10Mbps)
	TCPAddr        string // TCP listen address for fallback (e.g. ":4700"), empty = disabled
	MaxRooms       int    // max auto-created rooms in multi-room mode (0 = default 64)
	Verbose        bool   // enable verbose/debug logging
}

const defaultMaxRooms = 64

// New creates a new Server. Call Run() to start it.
func New(cfg Config) (*Server, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", cfg.Addr)
	if err != nil {
		return nil, err
	}

	// Bind to IPv6 wildcard with "udp" network to get a dual-stack socket
	// that accepts traffic on both IPv4 and IPv6. When a specific IPv4 or
	// IPv6 address is given, use the corresponding single-family socket.
	network := "udp"
	if udpAddr.IP == nil {
		udpAddr.IP = net.IPv6zero
	} else if udpAddr.IP.To4() == nil {
		network = "udp6"
	}
	conn, err := net.ListenUDP(network, udpAddr)
	if err != nil {
		return nil, err
	}

	// 校验子网大小: 当前 IP 分配仅支持 /24
	ones, bits := cfg.Subnet.Mask.Size()
	if bits != 32 || ones != 24 {
		conn.Close()
		return nil, fmt.Errorf("%s", i18n.Format(i18n.T().ServerSubnetMust, ones))
	}

	// Worker pool: 1 worker per 2 players, clamped to [16, 64]
	workers := cfg.MaxPlayers / 2
	if workers < 16 {
		workers = 16
	}
	if workers > 64 {
		workers = 64
	}

	// Channel buffer: scale with player count for burst absorption
	chanBuf := cfg.MaxPlayers * 128
	if chanBuf < 8192 {
		chanBuf = 8192
	}

	// Tune UDP socket buffers (ignoring error on non-Linux platforms)
	if err := netutil.SetSocketBuffers(conn); err != nil {
		log.Printf("[server] set socket buffers: %v (using OS defaults)", err)
	}

	maxPerIP := cfg.MaxPerIP
	if maxPerIP <= 0 {
		maxPerIP = 3
	}

	bwLimiter := NewBandwidthLimiter(cfg.BandwidthLimit)

	maxRooms := cfg.MaxRooms
	if maxRooms <= 0 && cfg.MultiRoom {
		maxRooms = defaultMaxRooms
	}

	s := &Server{
		conn:         conn,
		statusAddr:   cfg.StatusAddr,
		statusToken:  cfg.StatusToken,
		version:      cfg.Version,
		lang:         cfg.Lang,
		startTime:    time.Now(),
		verbose:      cfg.Verbose,
		workers:      workers,
		pktCh:        make(chan pktJob, chanBuf),
		rateShards:   newRateShardsArray(),
		rateTick:     time.NewTicker(rateInterval),
		metricsTS:    NewMetricsTimeSeries(),
		rooms:        make(map[string]*Room),
		addrToRoom:   make(map[netkey.RateKey]*Room),
		multiRoom:    cfg.MultiRoom,
		maxRooms:     maxRooms,
		stateDir:     cfg.StateDir,
		bwLimiter:    bwLimiter,
		roomPass:     cfg.RoomPass,
		ready:        make(chan struct{}),
		localSubnets: getLocalSubnets(),
	}

	// Wire TCP bridge routing into the send queue (must happen after s is
	// created so the closure can reference s.tcpBridges).
	tcpWrite := func(addr *net.UDPAddr, data []byte) bool {
		key := netkey.AddrToRateKey(addr)
		if b, ok := s.tcpBridges.Load(key); ok {
			if bridge, ok := b.(*netutil.UDPTCPBridge); ok {
				if err := bridge.Send(data); err != nil {
					log.Printf("[server] tcp bridge send error: %v", err)
				}
				return true
			}
			return false // sentinel ("reserved") — bridge not ready yet
		}
		return false
	}
	s.sendQueue = newRateLimitedQueue(conn, bwLimiter, tcpWrite)

	// Create default room for single-room mode
	if !cfg.MultiRoom {
		defaultRoom, err := NewRoom(RoomConfig{
			RoomID:     "default",
			RoomPass:   cfg.RoomPass,
			Subnet:     cfg.Subnet,
			MaxPlayers: cfg.MaxPlayers,
			MaxPerIP:   maxPerIP,
			Conn:       conn,
			SendQueue:  s.sendQueue,
			BWLimiter:  bwLimiter,
			Verbose:    cfg.Verbose,
		})
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("failed to create default room: %w", err)
		}
		defaultRoom.onDirty = func() {
			s.persistDirty.Store(true)
		}
		s.defaultRoom = defaultRoom
		s.rooms["default"] = defaultRoom
	} else {
		// Multi-room mode: store the base subnet for allocateSubnet()
		s.baseSubnet = cfg.Subnet
	}

	// Cache the encrypted flag — true if any room uses password-based AEAD.
	// In single-room mode, check the default room; in multi-room mode, all
	// rooms share the server password.
	if s.defaultRoom != nil && s.defaultRoom.roomPass != "" {
		s.cachedEncrypted = true
	} else if s.multiRoom && s.roomPass != "" {
		s.cachedEncrypted = true
	}

	// Load persisted state (if any)
	if err := s.loadState(); err != nil {
		log.Printf("warning: failed to load state: %v", err)
	}

	// Start TCP fallback listener if configured
	if cfg.TCPAddr != "" {
		tcpLn, err := netutil.NewTCPListener(cfg.TCPAddr)
		if err != nil {
			log.Printf("[server] TCP listener failed to start: %v (TCP fallback disabled)", err)
		} else {
			s.tcpListener = tcpLn
			log.Printf("[server] TCP fallback listener on %s", tcpLn.Addr())
		}
	}

	return s, nil
}

// Run starts the server and blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) {
	s.closeMu.Lock()
	s.ctx, s.cancelCtx = context.WithCancel(ctx)
	s.closeMu.Unlock()
	s.startStatusServer(ctx, s.statusAddr)
	go s.keepaliveLoop(ctx)
	go s.rateLimitLoop(ctx)
	go s.persistLoop(ctx)
	go s.metricsLoop(ctx)
	go s.bwCleanupLoop(ctx)
	go s.sendQueue.run(ctx) // priority send queue

	// Start room-specific loops
	s.roomMu.RLock()
	for _, room := range s.rooms {
		go room.peerInfoLoop(ctx)
		go room.pingLoop(ctx)
	}
	s.roomMu.RUnlock()

	for i := 0; i < s.workers; i++ {
		go s.worker(ctx)
	}

	// Start TCP accept loop if TCP listener is enabled
	if s.tcpListener != nil {
		go s.tcpAcceptLoop(ctx)
	}

	// Track the main read loop for clean shutdown.
	// Add(1) must happen before close(s.ready) so that Close() never
	// observes an empty WaitGroup while the read loop is still starting.
	s.runWg.Add(1)
	defer s.runWg.Done()

	// Signal readiness — the server's UDP socket is bound and all
	// background goroutines have been launched.
	close(s.ready)

	buf := make([]byte, 65535)
	for {
		n, remoteAddr, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				log.Printf("[server] read error: %v", err)
				return // connection error (e.g. closed) — exit loop to avoid busy-spin
			}
		}
		if n < protocol.HeaderLen+protocol.ChecksumLen {
			continue
		}

		if !s.checkRate(remoteAddr) {
			s.totalPacketsDropped.Add(1)
			s.logDrop("rate limit")
			continue
		}

		pkt := pool.PktBufGet(n)
		n2 := copy(pkt, buf[:n])

		select {
		case s.pktCh <- pktJob{data: pkt[:n2], addr: remoteAddr}:
		default:
			// channel full — drop (backpressure), return buffer to pool
			pool.PktBufPut(pkt)
			s.totalPacketsDropped.Add(1)
			s.logDrop("channel full")
		}
	}
}

// Close shuts down the server and all room background goroutines.
// Sends disconnect notifications to all connected clients before closing.
func (s *Server) Close() error {
	// Notify all clients BEFORE cancelling context so sendQueue is still running
	s.roomMu.RLock()
	for _, room := range s.rooms {
		room.notifyShutdown()
	}
	s.roomMu.RUnlock()

	// Cancel context to signal all goroutines to exit.
	// This triggers sendQueue.run() to drain() all pending packets (including
	// the disconnect notifications just sent) before exiting.
	s.closeMu.Lock()
	cancel := s.cancelCtx
	s.cancelCtx = nil
	s.closeMu.Unlock()
	if cancel != nil {
		cancel()
	}

	// Stop the sendQueue and wait for it to drain all pending packets
	// (including the disconnect notifications just sent).
	s.sendQueue.Stop()
	s.sendQueue.WaitDrain()

	// Close the UDP connection first to unblock ReadFromUDP in Run().
	// ReadFromUDP will return "use of closed network connection" error,
	// allowing Run() to exit and runWg.Wait() to complete.
	s.conn.Close()

	// Wait for the main read loop to exit.
	s.runWg.Wait()

	// Stop all rooms
	s.roomMu.RLock()
	for _, room := range s.rooms {
		room.Stop()
	}
	s.roomMu.RUnlock()

	// Close TCP fallback listener (if enabled)
	if s.tcpListener != nil {
		s.tcpListener.Close()
	}

	// Clean up all TCP bridges to prevent goroutine leak
	s.tcpBridges.Range(func(key, value interface{}) bool {
		bridge := value.(*netutil.UDPTCPBridge)
		bridge.Stop()
		s.tcpBridges.Delete(key)
		return true
	})

	// Close pprof listener (if enabled)
	if s.pprofListener != nil {
		s.pprofListener.Close()
	}

	return nil
}

// SetPprofListener sets the pprof HTTP listener for runtime profiling.
// Must be called before Run(). The listener is closed on Server.Close().
func (s *Server) SetPprofListener(l net.Listener) {
	s.pprofListener = l
}

// WaitReady blocks until the server's Run() has entered its main read loop
// and all background goroutines have been started.
func (s *Server) WaitReady() {
	<-s.ready
}

// logDrop logs a packet drop event: first drop, then every 1000 drops.
func (s *Server) logDrop(reason string) {
	total := s.totalPacketsDropped.Load()
	if total == 1 || total%1000 == 0 {
		log.Printf("[server] packet dropped (%s): total=%d", reason, total)
	}
}

// ── Worker Pool ────────────────────────────────────────────────

func (s *Server) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-s.pktCh:
			s.handlePacket(job.data, job.addr)
			// Return buffer to pool with its original capacity.
			pool.PktBufPut(job.data[:cap(job.data)])
		}
	}
}

// ── Packet Dispatch ────────────────────────────────────────────

func (s *Server) handlePacket(data []byte, from *net.UDPAddr) {
	// Use the cached encrypted flag (computed once in New()).
	encrypted := s.cachedEncrypted

	// Decode: use DecodeLenient for encrypted rooms to handle both
	// CRC32-wrapped (from EncodeChecked clients) and bare (from Encode
	// clients) packets. This strips CRC32 when present, preventing 4
	// extra bytes from corrupting the relay payload written to TUN.
	// Pre-auth packets (Register, NATProbe, AuthResponse, Rebind) are always
	// unencrypted — verify their CRC in the encrypted fast path.
	var msg *protocol.Message
	if encrypted {
		var err error
		msg, err = protocol.DecodeLenient(data, true)
		if msg == nil {
			if err != nil && !errors.Is(err, protocol.ErrPacketTooShort) {
				log.Printf("failed to decode encrypted packet: %v", err)
			}
			return
		}
		if msg.Type == protocol.TypeRegister ||
			msg.Type == protocol.TypeNATProbe ||
			msg.Type == protocol.TypeAuthResponse ||
			msg.Type == protocol.TypeRebind {
			if _, err := protocol.VerifyChecksum(data); err != nil {
				return
			}
		}
	} else {
		var err error
		msg, err = protocol.DecodeLenient(data, false)
		if err != nil {
			if errors.Is(err, protocol.ErrUnsupportedVersion) {
				s.sendKickCode(from, protocol.KickCodeVersionMismatch, fmt.Sprintf(
					"Protocol version mismatch: server=%d, please update your client",
					protocol.ProtocolVersion))
			}
			return
		}
	}

	// Pre-registration handlers (unencrypted, before room routing)
	switch msg.Type {
	case protocol.TypeNATProbe:
		s.handleNATProbe(msg.Payload, from)
	case protocol.TypeRebind:
		s.handleRebind(msg.Payload, from)
	default:
		if s.multiRoom {
			s.handlePacketMultiRoom(msg, from)
		} else if s.defaultRoom != nil {
			s.defaultRoom.HandlePacket(msg.Type, msg.Payload, from)
		}
	}
}

// ── Multi-Room Packet Routing ──────────────────────────────────

func (s *Server) handlePacketMultiRoom(msg *protocol.Message, from *net.UDPAddr) {
	fromKey := netkey.AddrToRateKey(from)

	// For Register, we need to parse roomID first to find/create the room
	if msg.Type == protocol.TypeRegister {
		s.handleRegisterMultiRoom(msg.Payload, from)
		return
	}

	// For all other types, look up the room from addrToRoom
	s.roomMu.RLock()
	room := s.addrToRoom[fromKey]
	s.roomMu.RUnlock()

	if room == nil {
		return // not registered in any room
	}

	// Room may have been stopped between the addrToRoom lookup and this
	// call (e.g. during server shutdown or stale room cleanup).
	select {
	case <-room.done:
		return
	default:
	}

	room.HandlePacket(msg.Type, msg.Payload, from)
}

func (s *Server) handleRegisterMultiRoom(payload []byte, from *net.UDPAddr) {
	reg, err := protocol.UnmarshalRegister(payload)
	if err != nil {
		return
	}

	if len(reg.RoomID) == 0 || len(reg.RoomID) > maxRoomIDLen {
		s.sendKick(from, i18n.T().KickInvalidRoom)
		return
	}

	// Find or create room
	s.roomMu.Lock()
	room, exists := s.rooms[reg.RoomID]
	if !exists {
		// Check room limit before creating
		if s.maxRooms > 0 && len(s.rooms) >= s.maxRooms {
			s.roomMu.Unlock()
			s.sendKick(from, "server room limit reached")
			return
		}
		// Auto-create room with default settings
		// Each room gets the next available /24 subnet
		subnet := s.allocateSubnet()
		if subnet == nil {
			s.roomMu.Unlock()
			s.sendKick(from, "no available subnets for new room")
			return
		}
		var err error
		room, err = NewRoom(RoomConfig{
			RoomID:     reg.RoomID,
			RoomPass:   s.roomPass, // propagate server password to auto-created rooms
			Subnet:     subnet,
			MaxPlayers: 254, // default max for multi-room
			MaxPerIP:   3,
			Conn:       s.conn,
			SendQueue:  s.sendQueue,
			BWLimiter:  s.bwLimiter,
			Verbose:    s.verbose,
		})
		if err != nil {
			s.roomMu.Unlock()
			s.sendKick(from, "failed to create room")
			return
		}
		s.rooms[reg.RoomID] = room
		log.Printf("[room] created room %q with subnet %s", reg.RoomID, subnet)
		// Start room lifecycle loops for the newly created room
		s.closeMu.Lock()
		ctx := s.ctx
		s.closeMu.Unlock()
		go room.peerInfoLoop(ctx)
		go room.pingLoop(ctx)
	}
	s.roomMu.Unlock()

	// Register client in the room
	fromKey := netkey.AddrToRateKey(from)

	room.HandlePacket(protocol.TypeRegister, payload, from)

	// If client was registered (has a client entry), add to addrToRoom.
	// Release room.mu before acquiring s.roomMu to avoid ABBA deadlock
	// with keepaliveLoop's multi-room cleanup.
	room.mu.RLock()
	registered := room.addrMap[fromKey] != nil
	room.mu.RUnlock()

	if registered {
		s.roomMu.Lock()
		s.addrToRoom[fromKey] = room
		s.roomMu.Unlock()
	}
}

// ── TCP Accept Loop ─────────────────────────────────────────────

// tcpAcceptLoop accepts TCP connections and bridges them to the UDP
// processing pipeline. This allows clients behind strict firewalls
// (that block UDP) to connect via TCP.
func (s *Server) tcpAcceptLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		tcp, err := s.tcpListener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				log.Printf("[server] TCP accept error: %v", err)
				continue
			}
		}

		log.Printf("[server] TCP client connected: %s", tcp.RemoteAddr())

		// Assign a unique port for this TCP client so its synthetic
		// address is distinct in rateKey/addrMap lookups.
		// Create bridge first, then atomically register it to avoid TOCTOU.
		var bridge *netutil.UDPTCPBridge
		var key netkey.RateKey
		var port int
		const maxPortAttempts = 65536
		for attempt := 0; attempt < maxPortAttempts; attempt++ {
			port = int(s.tcpPortCounter.Add(1) % 65536)
			if port == 0 {
				port = 1
			}
			syntheticAddr := &net.UDPAddr{
				IP:   net.IPv4(127, 0, 0, 254),
				Port: port,
			}
			key = netkey.AddrToRateKey(syntheticAddr)
			candidate := netutil.NewUDPTCPBridge(tcp, syntheticAddr)
			if _, loaded := s.tcpBridges.LoadOrStore(key, candidate); !loaded {
				bridge = candidate
				break
			}
			// Collision — try next port.
			candidate.Stop()
		}
		if bridge == nil {
			tcp.Close()
			log.Printf("[server] TCP bridge: no available ports")
			continue
		}

		// Handle TCP packets in a goroutine
		go func() {
			defer bridge.Stop()
			defer s.tcpBridges.Delete(key)
			bridge.ReceiveLoop(func(data []byte, addr *net.UDPAddr) {
				if len(data) < protocol.HeaderLen+protocol.ChecksumLen {
					return
				}
				if !s.checkRate(addr) {
					s.totalPacketsDropped.Add(1)
					s.logDrop("rate limit")
					return
				}
				pkt := pool.PktBufGet(len(data))
				copy(pkt, data)
				select {
				case s.pktCh <- pktJob{data: pkt[:len(data)], addr: addr}:
				default:
					pool.PktBufPut(pkt)
					s.totalPacketsDropped.Add(1)
					s.logDrop("channel full")
				}
			})
		}()
	}
}

// ── Send Helpers ───────────────────────────────────────────────

func (s *Server) sendChecked(typ byte, payload []byte, to *net.UDPAddr) {
	data := protocol.EncodeChecked(typ, payload)
	if !s.sendQueue.send(data, to, priorityHigh) {
		n := s.sendErrors.Add(1)
		if n&(n-1) == 0 {
			log.Printf("%s", i18n.Format(i18n.T().ServerSendFail, n, "queue full"))
		}
	}
}

func (s *Server) sendKick(to *net.UDPAddr, reason string) {
	s.sendKickCode(to, protocol.KickCodeNone, reason)
}

func (s *Server) sendKickCode(to *net.UDPAddr, code protocol.KickCode, reason string) {
	kick := &protocol.KickPayload{Reason: reason, Code: code}
	s.sendChecked(protocol.TypeKick, kick.Marshal(), to)
}

// bwCleanupLoop periodically removes stale bandwidth limiter buckets.
func (s *Server) bwCleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.bwLimiter.Cleanup(5 * time.Minute)
		}
	}
}
