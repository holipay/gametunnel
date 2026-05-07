// Package server implements the GameTunnel relay server.
//
// It accepts client registrations (with optional HMAC auth), relays game
// traffic between peers, and handles UDP broadcast forwarding for LAN
// game discovery.
package server

import (
	"context"
	"log"
	"math/bits"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/holipay/gametunnel/internal/protocol"
)

// ── Auth State ─────────────────────────────────────────────────

type authState int

const (
	authNone         authState = iota // no password required, or already authenticated
	authChallengeSent                 // challenge sent, waiting for response
)

// ── Client State ───────────────────────────────────────────────

// Client represents a connected player.
type Client struct {
	Username   string
	VirtualIP  net.IP
	PublicAddr *net.UDPAddr
	LastSeen   time.Time

	// Auth state (only used when server has a room password)
	auth        authState
	challenge   []byte    // 16-byte nonce
	challengeAt time.Time // for expiry
	authRoomID  string    // room ID from register request (for key derivation)
}

// ip4Key converts a 4-byte IPv4 address to a [4]byte map key.
func ip4Key(ip net.IP) [4]byte {
	ip4 := ip.To4()
	return [4]byte{ip4[0], ip4[1], ip4[2], ip4[3]}
}

// ── Server ─────────────────────────────────────────────────────

// Server is the GameTunnel relay server.
type Server struct {
	conn       *net.UDPConn
	clients    map[[4]byte]*Client // virtualIP [4]byte → Client
	addrMap    map[rateKey]*Client // client endpoint → Client (O(1) lookup)
	mu         sync.RWMutex       // protects clients + addrMap
	subnet     *net.IPNet
	maxPlayers int
	serverIP   net.IP
	ipBitmap   []uint64 // bitmap for O(1) IP allocation (256 bits for /24)
	roomPass   string   // room password (empty = no auth)
	statusAddr string   // HTTP status address, empty = disabled
	version    string
	startTime  time.Time

	// Worker pool
	workers int
	pktCh   chan pktJob

	// Rate limiting: per-client packet count per window
	rateMu    sync.Mutex
	rateCount map[rateKey]int
	rateTick  *time.Ticker

	// Auth flood protection
	pendingAuth int
	maxPending  int

	// Registration rate limiting
	regMu       sync.Mutex
	regCount    map[string]int
	regTick     *time.Ticker
	maxRegPerIP int

	// Diagnostics
	sendErrors atomic.Int64 // send failure counter
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
	StatusAddr string // HTTP status address (e.g. ":4701"), empty = disabled
	Version    string
}

// New creates a new Server. Call Run() to start it.
func New(cfg Config) (*Server, error) {
	udpAddr, err := net.ResolveUDPAddr("udp4", cfg.Addr)
	if err != nil {
		return nil, err
	}

	conn, err := net.ListenUDP("udp4", udpAddr)
	if err != nil {
		return nil, err
	}

	serverIP := make(net.IP, 4)
	copy(serverIP, cfg.Subnet.IP.To4())
	serverIP[3] = 1

	workers := 8
	if cfg.MaxPlayers > 20 {
		workers = 16
	}

	s := &Server{
		conn:        conn,
		clients:     make(map[[4]byte]*Client),
		addrMap:     make(map[rateKey]*Client),
		subnet:      cfg.Subnet,
		maxPlayers:  cfg.MaxPlayers,
		serverIP:    serverIP,
		ipBitmap:    make([]uint64, 4), // 256 bits for /24 subnet
		roomPass:    cfg.RoomPass,
		statusAddr:  cfg.StatusAddr,
		version:     cfg.Version,
		startTime:   time.Now(),
		workers:     workers,
		pktCh:       make(chan pktJob, 4096),
		rateCount:   make(map[rateKey]int),
		maxPending:  cfg.MaxPlayers * 3,
		regCount:    make(map[string]int),
		maxRegPerIP: 5,
	}
	s.markIPUsed(net.IPv4(serverIP[0], serverIP[1], serverIP[2], 0))   // network address
	s.markIPUsed(serverIP)                                                // server IP
	s.markIPUsed(net.IPv4(serverIP[0], serverIP[1], serverIP[2], 255))  // broadcast
	return s, nil
}

// Run starts the server and blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) {
	s.startStatusServer(ctx, s.statusAddr)
	go s.keepaliveLoop(ctx)
	go s.rateLimitLoop(ctx)
	go s.regRateLimitLoop(ctx)

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
			continue
		}

		pkt := make([]byte, n)
		copy(pkt, buf[:n])

		select {
		case s.pktCh <- pktJob{data: pkt, addr: remoteAddr}:
		default:
			// channel full — drop (backpressure)
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

		// Phase 1: scan under RLock (non-blocking for packet processing)
		s.mu.RLock()
		var staleClients [][4]byte
		var staleAuth []rateKey
		for key, c := range s.clients {
			if now.Sub(c.LastSeen) > 45*time.Second {
				staleClients = append(staleClients, key)
			}
		}
		for addrKey, c := range s.addrMap {
			if c.auth == authChallengeSent && now.Sub(c.challengeAt) > 30*time.Second {
				staleAuth = append(staleAuth, addrKey)
			}
		}
		s.mu.RUnlock()

		if len(staleClients) == 0 && len(staleAuth) == 0 {
			continue
		}

		// Phase 2: delete under WLock (re-verify stale entries)
		s.mu.Lock()
		changed := false
		for _, key := range staleClients {
			if c, ok := s.clients[key]; ok && now.Sub(c.LastSeen) > 45*time.Second {
				log.Printf("[-] %s (%s) 超时断开", c.Username, c.VirtualIP)
				s.markIPFree(c.VirtualIP)
				delete(s.clients, key)
				delete(s.addrMap, addrToRateKey(c.PublicAddr))
				changed = true
			}
		}
		for _, addrKey := range staleAuth {
			if c, ok := s.addrMap[addrKey]; ok && c.auth == authChallengeSent && now.Sub(c.challengeAt) > 30*time.Second {
				delete(s.addrMap, addrKey)
				s.pendingAuth--
			}
		}
		s.mu.Unlock()

		if changed {
			s.sendPeerInfoTo(nil, nil)
		}
	}
}

// ── Send Helpers ───────────────────────────────────────────────

func (s *Server) sendChecked(typ byte, payload []byte, to *net.UDPAddr) {
	data := protocol.EncodeChecked(typ, payload)
	if _, err := s.conn.WriteToUDP(data, to); err != nil {
		if s.sendErrors.Add(1) == 1 {
			log.Printf("[server] 发送失败: %v", err)
		}
	}
}

func (s *Server) sendCheckedRaw(data []byte, to *net.UDPAddr) {
	if _, err := s.conn.WriteToUDP(data, to); err != nil {
		if s.sendErrors.Add(1) == 1 {
			log.Printf("[server] 发送失败: %v", err)
		}
	}
}

func (s *Server) sendKick(to *net.UDPAddr, reason string) {
	kick := &protocol.KickPayload{Reason: reason}
	s.sendChecked(protocol.TypeKick, kick.Marshal(), to)
}

func (s *Server) sendAssignIP(vip net.IP, to *net.UDPAddr) {
	assign := &protocol.AssignIPPayload{
		VirtualIP:  vip,
		SubnetMask: s.subnet.Mask,
		ServerIP:   s.serverIP,
	}
	s.sendChecked(protocol.TypeAssignIP, assign.Marshal(), to)
}

// ── Types ──────────────────────────────────────────────────────

type peerSnapshot struct {
	virtualIP  net.IP
	publicAddr *net.UDPAddr
	username   string
}
