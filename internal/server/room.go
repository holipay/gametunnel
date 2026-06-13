package server

import (
	"fmt"
	"log"
	"math/bits"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/holipay/gametunnel/internal/auth"
	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/protocol"
)

// connIPKey is a fixed-size key for per-IP connection counting.
// Uses the raw 4-byte IPv4 address to avoid string allocation per packet.
type connIPKey [4]byte

func addrToConnIPKey(addr *net.UDPAddr) connIPKey {
	ip4 := addr.IP.To4()
	var k connIPKey
	if ip4 != nil {
		copy(k[:], ip4)
	}
	return k
}

// Room holds all per-room state. Each room has an independent virtual subnet,
// player list, IP allocation, and authentication.
type Room struct {
	roomID     string
	roomPass   string
	subnet     *net.IPNet
	serverIP   net.IP
	maxPlayers int

	// Network
	conn       *net.UDPConn      // UDP connection for sending packets
	bwLimiter  *BandwidthLimiter // per-client outbound bandwidth limiter

	// Per-room client state
	clients    map[[16]byte]*Client // virtualIP → Client
	addrMap    map[rateKey]*Client  // client endpoint → Client
	mu         sync.RWMutex

	// IP allocation (per-room, independent)
	ipBitmap []uint64

	// Auth
	authKeys    sync.Map // map[string][]byte, roomID → derived key
	pendingAuth int
	maxPending  int

	// Registration rate limiting (per-room)
	regMu       sync.Mutex
	regBuf      [2]map[string]int
	regTick     *time.Ticker
	done        chan struct{}
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
	createdAt time.Time
}

// RoomConfig holds configuration for creating a new room.
type RoomConfig struct {
	RoomID     string
	RoomPass   string
	Subnet     *net.IPNet
	MaxPlayers int
	MaxPerIP   int
	Conn       *net.UDPConn      // UDP connection for sending packets
	BWLimiter  *BandwidthLimiter // per-client outbound bandwidth limiter (optional)
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
		bwLimiter:   cfg.BWLimiter,
		clients:     make(map[[16]byte]*Client),
		addrMap:     make(map[rateKey]*Client),
		ipBitmap:    make([]uint64, 4),
		maxPending:  cfg.MaxPlayers * 3,
		regBuf:      [2]map[string]int{make(map[string]int), make(map[string]int)},
		maxRegPerIP: 5,
		ipConnCount: make(map[connIPKey]int),
		maxPerIP:    maxPerIP,
		done:        make(chan struct{}),
		createdAt:   time.Now(),
	}

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
	octet := ip.To4()[3]
	r.ipBitmap[octet/64] |= 1 << (octet % 64)
}

func (r *Room) markIPFree(ip net.IP) {
	octet := ip.To4()[3]
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

func (r *Room) checkRegRate(ip string) bool {
	r.regMu.Lock()
	r.regBuf[0][ip]++
	ok := r.regBuf[0][ip] <= r.maxRegPerIP
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

// Stop shuts down per-room background goroutines.
func (r *Room) Stop() {
	close(r.done)
	r.regTick.Stop()
}

// ── Auth ───────────────────────────────────────────────────────

func (r *Room) getAuthKey(roomID string) []byte {
	if v, ok := r.authKeys.Load(roomID); ok {
		return v.([]byte)
	}
	key := auth.DeriveKey(r.roomPass, roomID)
	if key != nil {
		r.authKeys.Store(roomID, key)
	}
	return key
}

// ── Packet Handling ────────────────────────────────────────────

// HandlePacket dispatches a packet within this room.
func (r *Room) HandlePacket(msgType byte, payload []byte, from *net.UDPAddr) {
	switch msgType {
	case protocol.TypeRegister:
		r.handleRegister(payload, from)
	case protocol.TypeAuthResponse:
		r.handleAuthResponse(payload, from)
	case protocol.TypeKeepAlive:
		r.handleKeepAlive(from)
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
	}
}

// ── Send Helpers ───────────────────────────────────────────────

// sendErrorLogInterval is how often to log send errors (rate limiting).
const sendErrorLogInterval = 1 * time.Minute

func (r *Room) sendChecked(typ byte, payload []byte, to *net.UDPAddr) {
	data := protocol.EncodeChecked(typ, payload)
	if _, err := r.conn.WriteToUDP(data, to); err != nil {
		r.sendErrors.Add(1)
		r.logSendError(err)
	}
}

func (r *Room) sendCheckedRaw(data []byte, to *net.UDPAddr) {
	if _, err := r.conn.WriteToUDP(data, to); err != nil {
		r.sendErrors.Add(1)
		r.logSendError(err)
	}
}

// logSendError logs send errors with rate limiting.
// Instead of logging every error (or only powers of 2), it logs
// a summary every sendErrorLogInterval when errors are occurring.
func (r *Room) logSendError(err error) {
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
			fmt.Sprintf("(%d errors in last minute, last: %v)", count, err))
	}
}

func (r *Room) sendKick(to *net.UDPAddr, reason string) {
	kick := &protocol.KickPayload{Reason: reason}
	r.sendChecked(protocol.TypeKick, kick.Marshal(), to)
	r.totalKicks.Add(1)
}

func (r *Room) sendAssignIP(vip net.IP, to *net.UDPAddr) {
	assign := &protocol.AssignIPPayload{
		VirtualIP:  vip,
		SubnetMask: r.subnet.Mask,
		ServerIP:   r.serverIP,
	}
	r.sendChecked(protocol.TypeAssignIP, assign.Marshal(), to)
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
