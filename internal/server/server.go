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

	"github.com/holipay/gametunnel/internal/protocol"
	"github.com/holipay/gametunnel/internal/i18n"
)

// pktPool reuses byte buffers for incoming packets to reduce GC pressure.
// Buffers are returned to the pool after each worker finishes processing.
var pktPool = sync.Pool{
	New: func() interface{} { return make([]byte, 65535) },
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
	LastSeen   time.Time
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
// IPv4 addresses are automatically mapped to v4-in-v6 format (::ffff:x.x.x.x).
func ipKey(ip net.IP) [16]byte {
	var k [16]byte
	copy(k[:], ip.To16())
	return k
}

// ── Server ─────────────────────────────────────────────────────

// Server is the GameTunnel relay server.
type Server struct {
	conn       *net.UDPConn
	statusAddr  string   // HTTP status address, empty = disabled
	statusToken string   // status page access token, empty = no auth
	version     string
	lang       i18n.Lang
	startTime  time.Time
	ctx        context.Context // stored for use in packet handlers

	// Worker pool
	workers int
	pktCh   chan pktJob

	// Rate limiting: per-client packet count per window
	rateMu    sync.Mutex
	rateBuf   [2]map[rateKey]int // double-buffer: [0]=active, [1]=stale
	rateTick  *time.Ticker

	// Time-series metrics
	metricsTS    *MetricsTimeSeries

	// Multi-room support
	multiRoom   bool
	rooms       map[string]*Room    // roomID → Room
	addrToRoom  map[rateKey]*Room   // client addr → Room (fast routing)
	roomMu      sync.RWMutex        // protects rooms + addrToRoom

	// Default room (single-room mode)
	defaultRoom *Room

	// Bandwidth limiting
	bwLimiter    *BandwidthLimiter // per-client outbound bandwidth limiter

	// Diagnostics
	sendErrors atomic.Int64 // send failure counter

	// State persistence
	stateDir      string       // directory for state file, empty = disabled
	stateLoadedAt time.Time    // when state was last loaded from disk
	persistDirty  atomic.Bool  // true if state needs to be written to disk

	// Operational metrics (lifetime counters, never reset)
	totalRegistrations atomic.Uint64 // successful joins
	authFailures       atomic.Uint64 // wrong password attempts
	peakPlayers        atomic.Uint32 // high watermark of concurrent players
	totalPacketsRelay  atomic.Uint64 // packets relayed (unicast + broadcast)
	totalPacketsDropped atomic.Uint64 // packets dropped (rate limit, full channel, invalid)
	totalKicks         atomic.Uint64 // clients kicked (rate limit, room full, auth fail, etc.)
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
}

// New creates a new Server. Call Run() to start it.
func New(cfg Config) (*Server, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", cfg.Addr)
	if err != nil {
		return nil, err
	}

	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}

	// 校验子网大小: 当前 IP 分配仅支持 /24
	ones, bits := cfg.Subnet.Mask.Size()
	if bits != 32 || ones != 24 {
		conn.Close()
		return nil, fmt.Errorf("%s", i18n.Format(i18n.T().ServerSubnetMust, ones))
	}

	// Worker pool: 1 worker per 4 players, clamped to [8, 32]
	workers := cfg.MaxPlayers / 4
	if workers < 8 {
		workers = 8
	}
	if workers > 32 {
		workers = 32
	}

	// Channel buffer: scale with player count for burst absorption
	chanBuf := cfg.MaxPlayers * 64
	if chanBuf < 4096 {
		chanBuf = 4096
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
		statusAddr:  cfg.StatusAddr,
		statusToken: cfg.StatusToken,
		version:     cfg.Version,
		lang:        cfg.Lang,
		startTime:   time.Now(),
		workers:     workers,
		pktCh:       make(chan pktJob, chanBuf),
		rateBuf:     [2]map[rateKey]int{make(map[rateKey]int), make(map[rateKey]int)},
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
	}

	// Load persisted state (if any)
	if err := s.loadState(); err != nil {
		log.Printf("warning: failed to load state: %v", err)
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

	buf := make([]byte, 65535)
	for {
		n, remoteAddr, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				return // connection error (e.g. closed) — exit loop to avoid busy-spin
			}
		}
		if n < 1 {
			continue
		}

		if !s.checkRate(remoteAddr) {
			s.totalPacketsDropped.Add(1)
			continue
		}

		pkt := pktPool.Get().([]byte)
		n2 := copy(pkt, buf[:n])

		select {
		case s.pktCh <- pktJob{data: pkt[:n2], addr: remoteAddr}:
		default:
			// channel full — drop (backpressure), return buffer to pool
			pktPool.Put(pkt)
			s.totalPacketsDropped.Add(1)
		}
	}
}

// Close shuts down the server.
func (s *Server) Close() error {
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
			// Return buffer to pool. Slice to full capacity so the next
			// Get() gets a usable 65535-byte buffer.
			pktPool.Put(job.data[:cap(job.data)])
		}
	}
}

// ── Packet Dispatch ────────────────────────────────────────────

func (s *Server) handlePacket(data []byte, from *net.UDPAddr) {
	msg, err := protocol.DecodeChecked(data)
	if err != nil {
		if errors.Is(err, protocol.ErrUnsupportedVersion) {
			s.sendKick(from, fmt.Sprintf(
				"Protocol version mismatch: server=%d, please update your client",
				protocol.ProtocolVersion))
		}
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

	// If client was registered (has a client entry), add to addrToRoom
	room.mu.RLock()
	if c := room.addrMap[fromKey]; c != nil {
		s.roomMu.Lock()
		s.addrToRoom[fromKey] = room
		s.roomMu.Unlock()
	}
	room.mu.RUnlock()
}

// allocateSubnet finds an unused /24 subnet for a new room.
// Uses 10.10.{room_index}.0/24 starting from 10.10.2.0.
func (s *Server) allocateSubnet() *net.IPNet {
	// Derive room subnets from the default room's subnet prefix.
	// e.g. server -subnet 192.168.1.0/24 → rooms get 192.168.2.0/24, 192.168.3.0/24, ...
	if s.defaultRoom == nil {
		return nil
	}
	base := s.defaultRoom.subnet.IP.To4()
	if base == nil {
		return nil
	}

	// Find the highest used 3rd octet
	maxIdx := int(base[2])
	if maxIdx < 1 {
		maxIdx = 1
	}
	for _, room := range s.rooms {
		octet := int(room.subnet.IP.To4()[2])
		if octet > maxIdx {
			maxIdx = octet
		}
	}
	nextIdx := maxIdx + 1
	if nextIdx > 254 {
		return nil // no more subnets
	}
	ip := net.IPv4(base[0], base[1], byte(nextIdx), 0)
	mask := net.CIDRMask(24, 32)
	return &net.IPNet{IP: ip, Mask: mask}
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
				room.peerInfoDirty.Store(true)
			}
		}

		// Clean up stale addrToRoom entries in multi-room mode.
		// When clients disconnect, the room removes from addrMap but
		// addrToRoom is only cleaned here to avoid cross-package coupling.
		if s.multiRoom {
			s.roomMu.Lock()
			for addrKey, room := range s.addrToRoom {
				room.mu.RLock()
				c := room.addrMap[addrKey]
				room.mu.RUnlock()
				if c == nil {
					delete(s.addrToRoom, addrKey)
				}
			}
			s.roomMu.Unlock()
		}
	}
}

// ── Send Helpers ───────────────────────────────────────────────

func (s *Server) sendChecked(typ byte, payload []byte, to *net.UDPAddr) {
	data := protocol.EncodeChecked(typ, payload)
	if _, err := s.conn.WriteToUDP(data, to); err != nil {
		n := s.sendErrors.Add(1)
		if n&(n-1) == 0 {
			log.Printf("%s", i18n.Format(i18n.T().ServerSendFail, n, err))
		}
	}
}

func (s *Server) sendKick(to *net.UDPAddr, reason string) {
	kick := &protocol.KickPayload{Reason: reason}
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
