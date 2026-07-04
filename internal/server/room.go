package server

import (
	"github.com/holipay/gametunnel/internal/netkey"
	"fmt"
	"log"
	"math/bits"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/protocol"
)

// ── Room Constants ──────────────────────────────────────────────

const (
	maxUsernameLen = 32
	maxRoomIDLen   = 32

	// peerInfoInterval is how often the batch PeerInfo broadcast runs.
	// 50ms coalesces up to ~20 join/leave events per broadcast.
	peerInfoInterval  = 50 * time.Millisecond
	peerInfoCacheTTL  = peerInfoInterval
	pingInterval      = 5 * time.Second
	sendErrorLogInterval = 1 * time.Minute

	// maxInlineTargets is the number of peer addresses we can hold on the stack
	// without heap allocation. For rooms ≤ 32 players this covers the common case.
	maxInlineTargets = 32

	// roomIdleTimeout is how long an empty multi-room can exist before being
	// cleaned up. Prevents goroutine leaks from transient rooms (players join
	// then leave, leaving peerInfoLoop and pingLoop running forever).
	roomIdleTimeout = 5 * time.Minute
)

// connIPKey is a fixed-size key for per-IP connection counting.
// Uses 16-byte IP to support both IPv4 (as v4-in-v6 mapped) and IPv6 addresses.
type connIPKey [16]byte

func addrToConnIPKey(addr *net.UDPAddr) connIPKey {
	var k connIPKey
	copy(k[:], addr.IP.To16())
	return k
}

// Room holds all per-room state. Each room has an independent virtual subnet,
// player list, IP allocation, and authentication.
type Room struct {
	roomID     string
	roomPass   string
	subnet     *net.IPNet // immutable after Room creation; safe to read without lock
	serverIP   net.IP
	maxPlayers int

	// Network
	conn       *net.UDPConn      // UDP connection for sending packets
	sendQueue  *rateLimitedQueue // priority send queue (shared with Server)
	bwLimiter  *BandwidthLimiter // per-client outbound bandwidth limiter

	// Per-room client state
	clients    map[[16]byte]*Client // virtualIP → Client
	addrMap    map[netkey.RateKey]*Client  // client endpoint → Client
	mu         sync.RWMutex

	// IP allocation (per-room, independent)
	ipBitmap []uint64

	// Auth
	authKeys    sync.Map // map[string][]byte, roomID → derived key
	pendingAuth int
	maxPending  int

	// Registration rate limiting (per-room)
	regMu       sync.Mutex
	regBuf      [2]map[connIPKey]int
	regTick     *time.Ticker
	done        chan struct{}
	stopOnce    sync.Once
	maxRegPerIP int

	// Per-IP connection count (per-room)
	ipConnMu    sync.Mutex
	ipConnCount map[connIPKey]int
	maxPerIP    int

	// PeerInfo batching
	peerInfoDirty    atomic.Bool
	peerInfoMu       sync.Mutex
	peerInfoEncoded  []byte
	peerInfoCachedAt time.Time

	// Operational metrics (per-room)
	totalRegistrations  atomic.Uint64
	authFailures        atomic.Uint64
	peakPlayers         atomic.Uint32
	totalPacketsRelay   atomic.Uint64
	totalPacketsDropped atomic.Uint64
	totalKicks          atomic.Uint64
	sendErrors          atomic.Int64

	// Send error logging (rate-limited)
	lastSendErrorLog       time.Time
	sendErrorCountSinceLog atomic.Int64
	sendErrorMu            sync.Mutex

	// Callbacks (set by Server for state persistence)
	onDirty func() // called when room state changes (for persistence)

	// Timestamps
	createdAt    time.Time
	lastActivity atomic.Int64 // unix nano, updated on client join/leave

	// Debug
	verbose bool
}

// RoomConfig holds configuration for creating a new room.
type RoomConfig struct {
	RoomID     string
	RoomPass   string
	Subnet     *net.IPNet
	MaxPlayers int
	MaxPerIP   int
	Conn       *net.UDPConn         // UDP connection for sending packets
	SendQueue  *rateLimitedQueue    // priority send queue (shared with Server)
	BWLimiter  *BandwidthLimiter    // per-client outbound bandwidth limiter (optional)
	Verbose    bool                 // enable verbose/debug logging
}

// NewRoom creates a new room. The subnet must be /24.
func NewRoom(cfg RoomConfig) (*Room, error) {
	ones, bits := cfg.Subnet.Mask.Size()
	if bits != 32 || ones != 24 {
		return nil, fmt.Errorf("subnet must be /%d", ones)
	}

	serverIP := make(net.IP, 4)
	copy(serverIP, cfg.Subnet.IP.To4())
	serverIP[3] = 1

	maxPerIP := cfg.MaxPerIP
	if maxPerIP <= 0 {
		maxPerIP = 3
	}

	r := &Room{
		roomID:      cfg.RoomID,
		roomPass:    cfg.RoomPass,
		subnet:      cfg.Subnet,
		serverIP:    serverIP,
		maxPlayers:  cfg.MaxPlayers,
		conn:        cfg.Conn,
		sendQueue:   cfg.SendQueue,
		bwLimiter:   cfg.BWLimiter,
		clients:     make(map[[16]byte]*Client),
		addrMap:     make(map[netkey.RateKey]*Client),
		ipBitmap:    make([]uint64, 4),
		maxPending:  min(cfg.MaxPlayers*3, 512),
		regBuf:      [2]map[connIPKey]int{make(map[connIPKey]int), make(map[connIPKey]int)},
		maxRegPerIP: 5,
		ipConnCount: make(map[connIPKey]int),
		maxPerIP:    maxPerIP,
		done:        make(chan struct{}),
		createdAt:   time.Now(),
		verbose:     cfg.Verbose,
	}
	r.lastActivity.Store(time.Now().UnixNano())

	// Reserve network, server, and broadcast addresses
	r.markIPUsed(net.IPv4(serverIP[0], serverIP[1], serverIP[2], 0))
	r.markIPUsed(serverIP)
	r.markIPUsed(net.IPv4(serverIP[0], serverIP[1], serverIP[2], 255))

	// Start per-room reg rate limiter
	r.regTick = time.NewTicker(time.Second)
	go r.regRateLimitLoop()

	return r, nil
}

// ── IP Bitmap ──────────────────────────────────────────────────

func (r *Room) markIPUsed(ip net.IP) {
	ip4 := ip.To4()
	if ip4 == nil {
		return
	}
	octet := ip4[3]
	r.ipBitmap[octet/64] |= 1 << (octet % 64)
}

func (r *Room) markIPFree(ip net.IP) {
	ip4 := ip.To4()
	if ip4 == nil {
		return
	}
	octet := ip4[3]
	r.ipBitmap[octet/64] &^= 1 << (octet % 64)
}

func (r *Room) nextAvailableIP() net.IP {
	base := r.subnet.IP.To4()
	for i, word := range r.ipBitmap {
		if word == ^uint64(0) {
			continue
		}
		free := ^word
		for free != 0 {
			bit := bits.TrailingZeros64(free)
			octet := i*64 + bit
			if octet >= 2 && octet < 255 {
				return net.IPv4(base[0], base[1], base[2], byte(octet))
			}
			free &= free - 1 // clear lowest set bit
		}
	}
	return nil
}

// ── Registration Rate Limiting ─────────────────────────────────

func (r *Room) checkRegRate(addr *net.UDPAddr) bool {
	key := addrToConnIPKey(addr)
	r.regMu.Lock()
	r.regBuf[0][key]++
	ok := r.regBuf[0][key] <= r.maxRegPerIP
	r.regMu.Unlock()
	return ok
}

func (r *Room) regRateLimitLoop() {
	for {
		select {
		case <-r.done:
			return
		case <-r.regTick.C:
			r.regMu.Lock()
			r.regBuf[0], r.regBuf[1] = r.regBuf[1], r.regBuf[0]
			for k := range r.regBuf[1] {
				delete(r.regBuf[1], k)
			}
			r.regMu.Unlock()
		}
	}
}

// Stop shuts down per-room background goroutines. Safe to call multiple times.
func (r *Room) Stop() {
	r.stopOnce.Do(func() {
		close(r.done)
		r.regTick.Stop()
	})
}

// ── Auth ───────────────────────────────────────────────────────

// ── Packet Handling ────────────────────────────────────────────

// HandlePacket dispatches a packet within this room.
func (r *Room) HandlePacket(msgType byte, payload []byte, from *net.UDPAddr) {
	switch msgType {
	case protocol.TypeRegister:
		r.handleRegister(payload, from)
	case protocol.TypeAuthResponse:
		r.handleAuthResponse(payload, from)
	case protocol.TypeKeepAlive:
		r.handleKeepAliveWithPayload(payload, from)
	case protocol.TypePeerRequest:
		r.handlePeerRequest(from)
	case protocol.TypeData:
		r.handleRelay(payload, from)
	case protocol.TypeHolePunch:
		r.handleHolePunch(payload, from)
	case protocol.TypeDisconnect:
		r.handleDisconnect(from)
	case protocol.TypePong:
		r.handlePong(payload, from)
	case protocol.TypeECDHConfirm:
		r.handleECDHConfirm(payload, from)
	}
}

// ── Send Helpers ───────────────────────────────────────────────

func (r *Room) sendChecked(typ byte, payload []byte, to *net.UDPAddr) {
	data := protocol.EncodeChecked(typ, payload)
	if !r.sendQueue.send(data, to, priorityHigh) {
		r.sendErrors.Add(1)
		r.logSendError("queue full")
	}
}

func (r *Room) sendCheckedRaw(data []byte, to *net.UDPAddr) {
	// Relay data is low priority; control packets use sendChecked.
	if !r.sendQueue.send(data, to, priorityLow) {
		r.sendErrors.Add(1)
		r.logSendError("queue full")
	}
}

// sendCheckedRawBypass sends relay data bypassing the bandwidth limiter.
// Used for broadcast relay packets (game discovery) that must reach all peers.
func (r *Room) sendCheckedRawBypass(data []byte, to *net.UDPAddr) {
	if !r.sendQueue.sendBypass(data, to) {
		r.sendErrors.Add(1)
		r.logSendError("queue full")
	}
}

// logSendError logs send errors with rate limiting.
// Instead of logging every error (or only powers of 2), it logs
// a summary every sendErrorLogInterval when errors are occurring.
func (r *Room) logSendError(errMsg string) {
	r.sendErrorCountSinceLog.Add(1)

	r.sendErrorMu.Lock()
	defer r.sendErrorMu.Unlock()

	now := time.Now()
	if now.Sub(r.lastSendErrorLog) < sendErrorLogInterval {
		return
	}

	count := r.sendErrorCountSinceLog.Swap(0)
	if count > 0 {
		r.lastSendErrorLog = now
		log.Printf(i18n.T().ServerSendFail, r.sendErrors.Load(),
			fmt.Sprintf("(%d errors in last minute, last: %s)", count, errMsg))
	}
}

func (r *Room) sendKick(to *net.UDPAddr, reason string) {
	r.sendKickCode(to, protocol.KickCodeNone, reason)
}

func (r *Room) sendKickCode(to *net.UDPAddr, code protocol.KickCode, reason string) {
	log.Printf("[kick] sending to %s: code=%d reason=%s", to, code, reason)
	kick := &protocol.KickPayload{Reason: reason, Code: code}
	r.sendChecked(protocol.TypeKick, kick.Marshal(), to)
	r.totalKicks.Add(1)
}

func (r *Room) sendAssignIP(vip net.IP, to *net.UDPAddr) {
	r.mu.RLock()
	c := r.clients[netkey.IPKey(vip)]
	var sessionToken [16]byte
	if c != nil {
		sessionToken = c.SessionToken
	}
	r.mu.RUnlock()

	assign := &protocol.AssignIPPayload{
		VirtualIP:    vip,
		SubnetMask:   r.subnet.Mask,
		ServerIP:     r.serverIP,
		Version:      protocol.AppVersion,
		SessionToken: sessionToken,
	}
	if data := assign.Marshal(); data != nil {
		r.sendChecked(protocol.TypeAssignIP, data, to)
	}
}

// markDirty notifies the server that room state has changed (for persistence).
func (r *Room) markDirty() {
	if r.onDirty != nil {
		r.onDirty()
	}
}

// ClientCount returns the number of authenticated clients in the room.
func (r *Room) ClientCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.clients)
}

// notifyShutdown sends a disconnect notification to all connected clients.
// Called during server shutdown so clients can detect the disconnection
// immediately instead of waiting for the keepalive timeout (30s).
func (r *Room) notifyShutdown() {
	r.mu.RLock()
	targets := make([]*net.UDPAddr, 0, len(r.clients))
	for _, c := range r.clients {
		if c.PublicAddr != nil {
			targets = append(targets, c.PublicAddr)
		}
	}
	r.mu.RUnlock()

	if len(targets) == 0 {
		return
	}
	kick := &protocol.KickPayload{Reason: "server shutdown", Code: protocol.KickCodeShutdown}
	payload := kick.Marshal()
	for _, addr := range targets {
		r.sendChecked(protocol.TypeKick, payload, addr)
	}
}

// decrementIPConnCount decrements the per-IP connection counter and removes
// the entry if it reaches zero. Caller must NOT hold r.mu (ipConnMu is internal).
func (r *Room) decrementIPConnCount(ip [16]byte) {
	r.ipConnMu.Lock()
	r.ipConnCount[ip]--
	if r.ipConnCount[ip] <= 0 {
		delete(r.ipConnCount, ip)
	}
	r.ipConnMu.Unlock()
}
