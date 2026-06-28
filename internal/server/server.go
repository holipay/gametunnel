// Package server implements the GameTunnel relay server.
//
// It accepts client registrations (with optional HMAC auth), relays game
// traffic between peers, and handles UDP broadcast forwarding for LAN
// game discovery.
package server

import (
	"context"
	"crypto/hmac"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/holipay/gametunnel/internal/auth"
	"github.com/holipay/gametunnel/internal/netutil"
	"github.com/holipay/gametunnel/internal/protocol"
	"github.com/holipay/gametunnel/internal/i18n"
)

// pktPools is a set of graded byte buffer pools for incoming packets.
// Size classes: 512B (most game packets), 2KB, 16KB, 65535B (max UDP).
// This reduces memory waste: a 100-byte game packet no longer consumes a
// 65535-byte buffer.
var pktPools = [4]*sync.Pool{
	{New: func() interface{} { b := make([]byte, 512); return &b }},
	{New: func() interface{} { b := make([]byte, 2048); return &b }},
	{New: func() interface{} { b := make([]byte, 16384); return &b }},
	{New: func() interface{} { b := make([]byte, 65535); return &b }},
}

func pktPoolGet(n int) []byte {
	var idx int
	switch {
	case n <= 512:
		idx = 0
	case n <= 2048:
		idx = 1
	case n <= 16384:
		idx = 2
	default:
		idx = 3
	}
	bp := pktPools[idx].Get().(*[]byte)
	return (*bp)[:cap(*bp)]
}

func pktPoolPut(buf []byte) {
	if buf == nil {
		return
	}
	c := cap(buf)
	switch c {
	case 512:
		pktPools[0].Put(&buf)
	case 2048:
		pktPools[1].Put(&buf)
	case 16384:
		pktPools[2].Put(&buf)
	case 65535:
		pktPools[3].Put(&buf)
	}
}

// ── Auth State ─────────────────────────────────────────────────

type authState int

const (
	authNone          authState = iota // no password required, or already authenticated
	authChallengeSent                  // challenge sent, waiting for response
)

// ── Client State ───────────────────────────────────────────────

// pingHistorySize is the number of recent ping results kept per client
// for loss rate and jitter calculation.
const pingHistorySize = 12

// Client represents a connected player.
type Client struct {
	Username   string
	VirtualIP  net.IP
	PublicAddr *net.UDPAddr
	lastSeen   atomic.Int64 // unix nano, use GetLastSeen/SetLastSeen
	RTT        time.Duration // latest round-trip latency

	// Ping quality stats (ring buffer of recent RTTs, 0 = missed)
	pingHistory  [pingHistorySize]time.Duration
	pingIdx      int       // next write position in pingHistory
	pingSeq      uint32    // monotonic ping sequence (for timeout detection)
	lastPingSent time.Time // when the last ping was sent
	lastPingSeq  uint32    // sequence of the last ping sent

	// Auth state (only used when server has a room password)
	auth        authState
	challenge   []byte    // 16-byte nonce
	challengeAt time.Time // for expiry
	authRoomID  string    // room ID from register request (for key derivation)
}

func (c *Client) GetLastSeen() time.Time {
	return time.Unix(0, c.lastSeen.Load())
}

func (c *Client) SetLastSeen(t time.Time) {
	c.lastSeen.Store(t.UnixNano())
}

// PingStats returns loss rate (0.0-1.0) and jitter from recent ping history.
func (c *Client) PingStats() (lossRate float64, jitter time.Duration) {
	total := c.pingIdx
	if total == 0 {
		return 0, 0
	}
	n := total
	if n > pingHistorySize {
		n = pingHistorySize
	}

	var received int
	var prevRTT time.Duration
	var jitterSum time.Duration
	var jitterCount int

	// Read from the ring buffer in chronological order: oldest entry first.
	// When pingIdx >= pingHistorySize, the oldest entry is at pingIdx % pingHistorySize.
	start := 0
	if total > pingHistorySize {
		start = total % pingHistorySize
	}
	for i := 0; i < n; i++ {
		rtt := c.pingHistory[(start+i)%pingHistorySize]
		if rtt == 0 {
			continue // missed
		}
		received++
		if prevRTT > 0 {
			diff := rtt - prevRTT
			if diff < 0 {
				diff = -diff
			}
			jitterSum += diff
			jitterCount++
		}
		prevRTT = rtt
	}

	lossRate = 1.0 - float64(received)/float64(n)
	if jitterCount > 0 {
		jitter = jitterSum / time.Duration(jitterCount)
	}
	return
}

// ipKey converts an IP address to a [16]byte map key.
// Delegates to netutil.IPKey for shared implementation.
func ipKey(ip net.IP) [16]byte {
	return netutil.IPKey(ip)
}

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
	ctx        context.Context // stored for use in packet handlers

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
	addrToRoom  map[rateKey]*Room   // client addr → Room (fast routing)
	roomMu      sync.RWMutex        // protects rooms + addrToRoom

	// Default room (single-room mode)
	defaultRoom *Room

	// Base subnet for multi-room mode (stored separately since defaultRoom is nil)
	baseSubnet *net.IPNet

	// Bandwidth limiting
	bwLimiter    *BandwidthLimiter // per-client outbound bandwidth limiter

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
}

// New creates a new Server. Call Run() to start it.
func New(cfg Config) (*Server, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", cfg.Addr)
	if err != nil {
		return nil, err
	}

	// Use "udp4" or "udp6" explicitly based on resolved address family.
	// "udp" with an IPv4 addr creates IPv4-only socket; with IPv6 it's
	// platform-dependent (dual-stack on Linux with bindv6only=0, but not guaranteed).
	network := "udp"
	if udpAddr.IP.To4() == nil {
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
	if err := setSocketBuffers(conn); err != nil {
		log.Printf("[server] set socket buffers: %v (using OS defaults)", err)
	}

	maxPerIP := cfg.MaxPerIP
	if maxPerIP <= 0 {
		maxPerIP = 3
	}

	bwLimiter := NewBandwidthLimiter(cfg.BandwidthLimit)

	s := &Server{
		conn:        conn,
		sendQueue:   newRateLimitedQueue(conn, bwLimiter),
		statusAddr:  cfg.StatusAddr,
		statusToken: cfg.StatusToken,
		version:     cfg.Version,
		lang:        cfg.Lang,
		startTime:   time.Now(),
		workers:     workers,
		pktCh:       make(chan pktJob, chanBuf),
		rateShards:  newRateShardsArray(),
		metricsTS:   NewMetricsTimeSeries(),
		rooms:       make(map[string]*Room),
		addrToRoom:  make(map[rateKey]*Room),
		multiRoom:   cfg.MultiRoom,
		stateDir:    cfg.StateDir,
		bwLimiter:   bwLimiter,
	}

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
	s.ctx = ctx
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
			continue
		}

		pkt := pktPoolGet(n)
		n2 := copy(pkt, buf[:n])

		select {
		case s.pktCh <- pktJob{data: pkt[:n2], addr: remoteAddr}:
		default:
			// channel full — drop (backpressure), return buffer to pool
			pktPoolPut(pkt)
			s.totalPacketsDropped.Add(1)
		}
	}
}

// Close shuts down the server and all room background goroutines.
// Sends disconnect notifications to all connected clients before closing.
func (s *Server) Close() error {
	// Notify all clients before shutting down
	s.roomMu.RLock()
	for _, room := range s.rooms {
		room.notifyShutdown()
		room.Stop()
	}
	s.roomMu.RUnlock()
	return s.conn.Close()
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
			pktPoolPut(job.data[:cap(job.data)])
		}
	}
}

// ── Packet Dispatch ────────────────────────────────────────────

func (s *Server) handlePacket(data []byte, from *net.UDPAddr) {
	// Fast path: encrypted packets have no CRC32 — skip the wasted
	// CRC compute by decoding directly. AEAD provides integrity.
	if s.defaultRoom != nil && s.defaultRoom.roomPass != "" {
		msg, _ := protocol.DecodeSkipCRC(data)
		if msg != nil {
			// NAT probes are unencrypted (sent pre-registration) — handle directly
			if msg.Type == protocol.TypeNATProbe {
				s.handleNATProbe(msg.Payload, from)
				return
			}
			// Rebind: client migrated to new address, reclaim session
			if msg.Type == protocol.TypeRebind {
				s.handleRebind(msg.Payload, from)
				return
			}
			if s.multiRoom {
				s.handlePacketMultiRoom(msg, from)
			} else {
				s.defaultRoom.HandlePacket(msg.Type, msg.Payload, from)
			}
			return
		}
	}

	msg, err := protocol.DecodeLenient(data)
	if err != nil {
		if errors.Is(err, protocol.ErrUnsupportedVersion) {
			s.sendKickCode(from, protocol.KickCodeVersionMismatch, fmt.Sprintf(
				"Protocol version mismatch: server=%d, please update your client",
				protocol.ProtocolVersion))
		}
		return
	}

	// NAT probe — handle before room routing (probes happen pre-registration)
	if msg.Type == protocol.TypeNATProbe {
		s.handleNATProbe(msg.Payload, from)
		return
	}

	// Rebind: client migrated to new address, reclaim session
	if msg.Type == protocol.TypeRebind {
		s.handleRebind(msg.Payload, from)
		return
	}

	if s.multiRoom {
		s.handlePacketMultiRoom(msg, from)
		return
	}

	// Single-room mode: route to default room
	if s.defaultRoom != nil {
		s.defaultRoom.HandlePacket(msg.Type, msg.Payload, from)
	}
}

// ── Multi-Room Packet Routing ──────────────────────────────────

func (s *Server) handlePacketMultiRoom(msg *protocol.Message, from *net.UDPAddr) {
	fromKey := addrToRateKey(from)

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
			RoomPass:   "", // multi-room mode uses per-room auth (future)
			Subnet:     subnet,
			MaxPlayers: 254, // default max for multi-room
			MaxPerIP:   3,
			Conn:       s.conn,
			SendQueue:  s.sendQueue,
			BWLimiter:  s.bwLimiter,
		})
		if err != nil {
			s.roomMu.Unlock()
			s.sendKick(from, "failed to create room")
			return
		}
		s.rooms[reg.RoomID] = room
		log.Printf("[room] created room %q with subnet %s", reg.RoomID, subnet)
		// Start room lifecycle loops for the newly created room
		go room.peerInfoLoop(s.ctx)
		go room.pingLoop(s.ctx)
	}
	s.roomMu.Unlock()

	// Register client in the room
	fromKey := addrToRateKey(from)

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

// allocateSubnet finds an unused /24 subnet for a new room.
// Uses 10.10.{room_index}.0/24 starting from 10.10.2.0.
// Skips subnets that overlap with any local network interface.
func (s *Server) allocateSubnet() *net.IPNet {
	// Derive room subnets from the base subnet prefix.
	// e.g. server -subnet 192.168.1.0/24 → rooms get 192.168.2.0/24, 192.168.3.0/24, ...
	var baseIP net.IP
	if s.defaultRoom != nil {
		baseIP = s.defaultRoom.subnet.IP.To4()
	} else if s.baseSubnet != nil {
		baseIP = s.baseSubnet.IP.To4()
	}
	if baseIP == nil {
		return nil
	}

	// Collect local interface subnets to avoid conflicts
	localSubnets := s.getLocalSubnets()

	// Find the highest used 3rd octet
	maxIdx := int(baseIP[2])
	if maxIdx < 1 {
		maxIdx = 1
	}
	for _, room := range s.rooms {
		octet := int(room.subnet.IP.To4()[2])
		if octet > maxIdx {
			maxIdx = octet
		}
	}

	// Scan for the next available subnet, skipping those that overlap
	// with local interfaces or already-used room subnets.
	for nextIdx := maxIdx + 1; nextIdx <= 254; nextIdx++ {
		candidate := &net.IPNet{
			IP:   net.IPv4(baseIP[0], baseIP[1], byte(nextIdx), 0),
			Mask: net.CIDRMask(24, 32),
		}
		if s.subnetOverlapsAny(candidate, localSubnets) {
			continue
		}
		return candidate
	}
	return nil // no more subnets
}

// getLocalSubnets returns all /24+ subnets assigned to local network interfaces.
func (s *Server) getLocalSubnets() []*net.IPNet {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var subnets []*net.IPNet
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ones, bits := ipNet.Mask.Size()
			if bits == 32 && ones >= 24 {
				subnets = append(subnets, ipNet)
			}
		}
	}
	return subnets
}

// subnetOverlapsAny checks if candidate overlaps with any subnet in the list.
func (s *Server) subnetOverlapsAny(candidate *net.IPNet, others []*net.IPNet) bool {
	for _, other := range others {
		if candidate.IP.Equal(other.IP) {
			return true
		}
		// Check if either network contains the other's IP
		if candidate.Contains(other.IP) || other.Contains(candidate.IP) {
			return true
		}
	}
	return false
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

		// Create a synthetic UDP address for this TCP client
		// so it can be identified in the server's address maps.
		syntheticAddr := &net.UDPAddr{
			IP:   net.IPv4(127, 0, 0, 254),
			Port: 0, // will be assigned uniquely
		}

		bridge := netutil.NewUDPTCPBridge(tcp, syntheticAddr)

		// Handle TCP packets in a goroutine
		go func() {
			defer bridge.Stop()
			bridge.ReceiveLoop(func(data []byte, addr *net.UDPAddr) {
				if len(data) < protocol.HeaderLen+protocol.ChecksumLen {
					return
				}
				if !s.checkRate(addr) {
					s.totalPacketsDropped.Add(1)
					return
				}
				pkt := pktPoolGet(len(data))
				copy(pkt, data)
				select {
				case s.pktCh <- pktJob{data: pkt[:len(data)], addr: addr}:
				default:
					pktPoolPut(pkt)
					s.totalPacketsDropped.Add(1)
				}
			})
		}()
	}
}

// ── Keepalive Loop ─────────────────────────────────────────────

func (s *Server) keepaliveLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// Clean up stale clients in all rooms
		s.roomMu.RLock()
		rooms := make([]*Room, 0, len(s.rooms))
		for _, r := range s.rooms {
			rooms = append(rooms, r)
		}
		s.roomMu.RUnlock()

		for _, room := range rooms {
			changed := room.CleanupStale()
			if changed {
				room.invalidatePeerInfoCache()
			}
		}

		// Clean up stale addrToRoom entries in multi-room mode.
		// When clients disconnect, the room removes from addrMap but
		// addrToRoom is only cleaned here to avoid cross-package coupling.
		if s.multiRoom {
			// Snapshot addrToRoom under one lock, then check each room once.
			s.roomMu.RLock()
			type roomEntry struct {
				key  rateKey
				room *Room
			}
			entries := make([]roomEntry, 0, len(s.addrToRoom))
			for k, room := range s.addrToRoom {
				if room != nil {
					entries = append(entries, roomEntry{key: k, room: room})
				}
			}
			s.roomMu.RUnlock()

			// Group by room to minimize lock acquisitions.
			byRoom := make(map[*Room][]rateKey)
			for _, e := range entries {
				byRoom[e.room] = append(byRoom[e.room], e.key)
			}

			var stale []rateKey
			for room, keys := range byRoom {
				room.mu.RLock()
				for _, k := range keys {
					if room.addrMap[k] == nil {
						stale = append(stale, k)
					}
				}
				room.mu.RUnlock()
			}

			if len(stale) > 0 {
				s.roomMu.Lock()
				for _, k := range stale {
					// Re-check under write lock: the entry may have been
					// replaced by a valid new registration (TOCTOU guard).
					if s.addrToRoom[k] != nil {
						// Find which room this key belongs to now
						room := s.addrToRoom[k]
						room.mu.RLock()
						stillStale := room.addrMap[k] == nil
						room.mu.RUnlock()
						if stillStale {
							delete(s.addrToRoom, k)
						}
					}
				}
				s.roomMu.Unlock()
			}

			// Clean up empty rooms that have been idle beyond the timeout.
			// This prevents goroutine leaks from transient rooms (players join
			// then leave, leaving peerInfoLoop and pingLoop running forever).
			s.roomMu.Lock()
			now := time.Now()
			for roomID, room := range s.rooms {
				if roomID == "default" {
					continue // never delete the default room
				}
				if room.ClientCount() > 0 {
					continue // room still has players
				}
				lastAct := time.Unix(0, room.lastActivity.Load())
				if now.Sub(lastAct) > roomIdleTimeout {
					room.Stop()
					delete(s.rooms, roomID)
					log.Printf("[room] cleaned up idle room %q (idle for %v)", roomID, now.Sub(lastAct))
				}
			}
			s.roomMu.Unlock()
		}
	}
}

// ── NAT Probe Handler ─────────────────────────────────────────────

// handleNATProbe responds to a client's NAT type probe request.
// The server includes the client's observed external address in the response,
// which the client uses to determine its NAT type.
//
// For multi-probe detection: the client sends multiple probes and compares
// the observed addresses. If they differ, it's a Symmetric NAT.
func (s *Server) handleNATProbe(payload []byte, from *net.UDPAddr) {
	probe, err := protocol.UnmarshalNATProbe(payload)
	if err != nil {
		return
	}

	// Build response with the client's observed address
	resp := &protocol.NATResponsePayload{
		ProbeID:      probe.ProbeID,
		NATType:      protocol.NATTypeUnknown, // client determines this from multiple probes
		ObservedAddr: from,
		AltAddr:      nil, // TODO: respond from alt port for better detection
	}

	s.sendChecked(protocol.TypeNATResponse, resp.Marshal(), from)
}

// ── Rebind Handler (Connection Migration) ───────────────────────

// handleRebind processes a client's address migration request.
// When a client's network changes (WiFi↔4G, NAT rebinding), it sends
// TypeRebind from the new address to reclaim its existing session.
//
// Security: if the room has a password, the client must provide a valid
// HMAC over its virtual IP. Without a password, the server relies on
// virtual IP matching + recent lastSeen (within 60s).
func (s *Server) handleRebind(payload []byte, from *net.UDPAddr) {
	req, err := protocol.UnmarshalRebind(payload)
	if err != nil {
		return
	}
	if req.VirtualIP.To4() == nil {
		s.sendRebindAck(from, false)
		return
	}

	vipKey := ipKey(req.VirtualIP)

	// Search all rooms for the client with this virtual IP
	var foundRoom *Room
	var foundClient *Client
	var clientAuthRoomID string
	var clientUsername string
	var clientLastSeen time.Time
	var clientPublicAddr *net.UDPAddr

	s.roomMu.RLock()
	for _, room := range s.rooms {
		room.mu.RLock()
		if c, ok := room.clients[vipKey]; ok {
			foundRoom = room
			foundClient = c
			clientAuthRoomID = c.authRoomID
			clientUsername = c.Username
			clientLastSeen = c.GetLastSeen()
			clientPublicAddr = c.PublicAddr
			room.mu.RUnlock()
			break
		}
		room.mu.RUnlock()
	}
	s.roomMu.RUnlock()

	if foundRoom == nil || foundClient == nil {
		s.sendRebindAck(from, false)
		return
	}

	// Verify HMAC if room has a password
	if foundRoom.roomPass != "" {
		if len(req.HMAC) == 0 {
			s.sendRebindAck(from, false)
			return
		}
		key := auth.DeriveKey(foundRoom.roomPass, clientAuthRoomID)
		if key == nil {
			s.sendRebindAck(from, false)
			return
		}
		// Verify HMAC — no address binding (rebind changes the address)
		expected := auth.ComputeHMAC(key, nil, clientAuthRoomID, clientUsername, nil)
		if !hmac.Equal(req.HMAC, expected) {
			s.sendRebindAck(from, false)
			return
		}
	} else {
		// No password — check that the client was recently active
		if time.Since(clientLastSeen) > 60*time.Second {
			s.sendRebindAck(from, false)
			return
		}
	}

	// Migration valid — update the client's address
	oldKey := addrToRateKey(clientPublicAddr)
	newKey := addrToRateKey(from)

	foundRoom.mu.Lock()
	// Remove old addrMap entry
	delete(foundRoom.addrMap, oldKey)
	// Update client address
	foundClient.PublicAddr = from
	foundClient.SetLastSeen(time.Now())
	foundRoom.addrMap[newKey] = foundClient
	foundRoom.lastActivity.Store(time.Now().UnixNano())
	foundRoom.mu.Unlock()

	// Update addrToRoom mapping in multi-room mode
	if s.multiRoom {
		s.roomMu.Lock()
		delete(s.addrToRoom, oldKey)
		s.addrToRoom[newKey] = foundRoom
		s.roomMu.Unlock()
	}

	log.Printf("[rebind] %s migrated: %v → %s", foundClient.Username, oldKey, from)
	s.sendRebindAck(from, true)

	// Send current peer info to the client on new address
	foundRoom.sendPeerInfoToClient(from)
}

func (s *Server) sendRebindAck(to *net.UDPAddr, success bool) {
	ack := &protocol.RebindAckPayload{Success: success}
	s.sendChecked(protocol.TypeRebindAck, ack.Marshal(), to)
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
			s.bwLimiter.Cleanup(10 * time.Minute)
		}
	}
}


