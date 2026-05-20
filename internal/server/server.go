// Package server implements the GameTunnel relay server.
//
// It accepts client registrations (with optional HMAC auth), relays game
// traffic between peers, and handles UDP broadcast forwarding for LAN
// game discovery.
package server

import (
	"context"
	"fmt"
	"log"
	"math/bits"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/holipay/gametunnel/internal/auth"
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
	clients    map[[16]byte]*Client // virtualIP [16]byte → Client
	addrMap    map[rateKey]*Client // client endpoint → Client (O(1) lookup)
	mu         sync.RWMutex        // protects clients + addrMap
	subnet     *net.IPNet
	maxPlayers int
	serverIP   net.IP
	ipBitmap   []uint64 // bitmap for O(1) IP allocation (256 bits for /24)
	roomPass    string   // room password (empty = no auth)
	statusAddr  string   // HTTP status address, empty = disabled
	statusToken string   // status page access token, empty = no auth
	version     string
	lang       i18n.Lang
	startTime  time.Time

	// Worker pool
	workers int
	pktCh   chan pktJob

	// PeerInfo batching: coalesce rapid join/leave into periodic broadcasts
	peerInfoDirty    atomic.Bool // set on join/leave, checked by peerInfoLoop
	peerInfoMu       sync.Mutex  // protects peerInfo cache
	peerInfoEncoded  []byte      // cached encoded PeerInfo packet
	peerInfoCachedAt time.Time   // when the cache was last refreshed

	// Rate limiting: per-client packet count per window
	rateMu    sync.Mutex
	rateBuf   [2]map[rateKey]int // double-buffer: [0]=active, [1]=stale
	rateTick  *time.Ticker

	// Cached auth keys (derived once per roomID, avoids repeated HKDF)
	authKeys sync.Map // map[string][]byte, roomID → derived key

	// Auth flood protection
	pendingAuth int
	maxPending  int

	// Registration rate limiting
	regMu       sync.Mutex
	regBuf      [2]map[string]int // double-buffer: [0]=active, [1]=stale
	regTick     *time.Ticker
	maxRegPerIP int

	// Per-IP connection count limit (prevents one IP from filling the room)
	ipConnMu   sync.Mutex
	ipConnCount map[string]int
	maxPerIP   int

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
	MaxPerIP   int    // max connections per IP (0 = use default 3)
	StateDir   string // directory for state persistence, empty = disabled
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

	serverIP := make(net.IP, 4)
	copy(serverIP, cfg.Subnet.IP.To4())
	serverIP[3] = 1

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
	setSocketBuffers(conn)

	s := &Server{
		conn:        conn,
		clients:     make(map[[16]byte]*Client),
		addrMap:     make(map[rateKey]*Client),
		subnet:      cfg.Subnet,
		maxPlayers:  cfg.MaxPlayers,
		serverIP:    serverIP,
		ipBitmap:    make([]uint64, 4), // 256 bits for /24 subnet
		roomPass:    cfg.RoomPass,
		statusAddr:  cfg.StatusAddr,
		statusToken: cfg.StatusToken,
		version:     cfg.Version,
		lang:        cfg.Lang,
		startTime:   time.Now(),
		workers:     workers,
		pktCh:       make(chan pktJob, chanBuf),
		rateBuf:     [2]map[rateKey]int{make(map[rateKey]int), make(map[rateKey]int)},
		maxPending:  cfg.MaxPlayers * 3,
		regBuf:      [2]map[string]int{make(map[string]int), make(map[string]int)},
		maxRegPerIP: 5,
		ipConnCount: make(map[string]int),
		maxPerIP:    cfg.MaxPerIP,
		stateDir:    cfg.StateDir,
	}
	if s.maxPerIP <= 0 {
		s.maxPerIP = 3
	}
	s.markIPUsed(net.IPv4(serverIP[0], serverIP[1], serverIP[2], 0))   // network address
	s.markIPUsed(serverIP)                                             // server IP
	s.markIPUsed(net.IPv4(serverIP[0], serverIP[1], serverIP[2], 255)) // broadcast

	// Load persisted state (if any)
	if err := s.loadState(); err != nil {
		log.Printf("warning: failed to load state: %v", err)
	}

	return s, nil
}

// Run starts the server and blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) {
	s.startStatusServer(ctx, s.statusAddr)
	go s.keepaliveLoop(ctx)
	go s.pingLoop(ctx)
	go s.rateLimitLoop(ctx)
	go s.regRateLimitLoop(ctx)
	go s.peerInfoLoop(ctx)
	go s.persistLoop(ctx)

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
				continue
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

// ServerIP returns the server's virtual IP.
func (s *Server) ServerIP() net.IP {
	return s.serverIP
}

// markIPUsed marks an IP address as taken in the allocation bitmap.
// MUST be called with s.mu held.
func (s *Server) markIPUsed(ip net.IP) {
	octet := ip.To4()[3]
	s.ipBitmap[octet/64] |= 1 << (octet % 64)
}

// markIPFree marks an IP address as available in the allocation bitmap.
// MUST be called with s.mu held.
func (s *Server) markIPFree(ip net.IP) {
	octet := ip.To4()[3]
	s.ipBitmap[octet/64] &^= 1 << (octet % 64)
}

// nextAvailableIP finds the next unallocated IP in the subnet using a bitmap.
// MUST be called with s.mu held. O(1) average case.
func (s *Server) nextAvailableIP() net.IP {
	base := s.subnet.IP.To4()
	for i, word := range s.ipBitmap {
		if word != ^uint64(0) {
			bit := bits.TrailingZeros64(^word)
			octet := i*64 + bit
			if octet >= 2 && octet < 255 {
				return net.IPv4(base[0], base[1], base[2], byte(octet))
			}
		}
	}
	return nil
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
		return
	}

	switch msg.Type {
	case protocol.TypeRegister:
		s.handleRegister(msg.Payload, from)
	case protocol.TypeAuthResponse:
		s.handleAuthResponse(msg.Payload, from)
	case protocol.TypeKeepAlive:
		s.handleKeepAlive(from)
	case protocol.TypePeerRequest:
		s.handlePeerRequest(from)
	case protocol.TypeData:
		s.handleRelay(msg.Payload, from)
	case protocol.TypeHolePunch:
		s.handleHolePunch(msg.Payload, from)
	case protocol.TypeDisconnect:
		s.handleDisconnect(from)
	case protocol.TypePong:
		s.handlePong(msg.Payload, from)
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

		now := time.Now()

		// Phase 1: scan under RLock — collect stale entries with all data
		// needed for deletion, so Phase 2 only does verify+delete (no re-scan).
		s.mu.RLock()
		type staleClient struct {
			key [16]byte
			c   *Client
		}
		type staleAuth struct {
			key rateKey
			c   *Client
		}
		var staleClients []staleClient
		var staleAuths []staleAuth
		for key, c := range s.clients {
			if now.Sub(c.LastSeen) > 30*time.Second {
				staleClients = append(staleClients, staleClient{key: key, c: c})
			}
		}
		for addrKey, c := range s.addrMap {
			if c.auth == authChallengeSent && now.Sub(c.challengeAt) > 30*time.Second {
				staleAuths = append(staleAuths, staleAuth{key: addrKey, c: c})
			}
		}
		s.mu.RUnlock()

		if len(staleClients) == 0 && len(staleAuths) == 0 {
			continue
		}

		// Phase 2: verify + delete under WLock (no re-scan, just check existence).
		s.mu.Lock()
		changed := false
		for _, sc := range staleClients {
			if cur, ok := s.clients[sc.key]; ok && cur == sc.c {
				log.Printf("%s", i18n.Format(i18n.T().ServerPeerLeave, sc.c.Username, sc.c.VirtualIP))
				s.markIPFree(sc.c.VirtualIP)
				delete(s.clients, sc.key)
				delete(s.addrMap, addrToRateKey(sc.c.PublicAddr))
				// Decrement per-IP connection count
				ip := sc.c.PublicAddr.IP.String()
				s.ipConnMu.Lock()
				s.ipConnCount[ip]--
				if s.ipConnCount[ip] <= 0 {
					delete(s.ipConnCount, ip)
				}
				s.ipConnMu.Unlock()
				changed = true
			}
		}
		for _, sa := range staleAuths {
			if cur, ok := s.addrMap[sa.key]; ok && cur == sa.c {
				delete(s.addrMap, sa.key)
				s.pendingAuth--
			}
		}
		s.mu.Unlock()

		if changed {
			s.peerInfoDirty.Store(true)
			s.markDirty()
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

func (s *Server) sendCheckedRaw(data []byte, to *net.UDPAddr) {
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
	s.totalKicks.Add(1)
}

func (s *Server) sendAssignIP(vip net.IP, to *net.UDPAddr) {
	assign := &protocol.AssignIPPayload{
		VirtualIP:  vip,
		SubnetMask: s.subnet.Mask,
		ServerIP:   s.serverIP,
	}
	s.sendChecked(protocol.TypeAssignIP, assign.Marshal(), to)
}

// getAuthKey returns the cached auth key for the given roomID, deriving it if needed.
func (s *Server) getAuthKey(roomID string) []byte {
	if v, ok := s.authKeys.Load(roomID); ok {
		return v.([]byte)
	}
	key := auth.DeriveKey(s.roomPass, roomID)
	if key != nil {
		s.authKeys.Store(roomID, key)
	}
	return key
}

// ── Types ──────────────────────────────────────────────────────

type peerSnapshot struct {
	virtualIP  net.IP
	publicAddr *net.UDPAddr
	username   string
}
