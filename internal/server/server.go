// Package server implements the GameTunnel relay server.
//
// It accepts client registrations (with optional HMAC auth), relays game
// traffic between peers, and handles UDP broadcast forwarding for LAN
// game discovery.
package server

import (
	"context"
	"log"
	"net"
	"sync"
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

// ── Server ─────────────────────────────────────────────────────

// Server is the GameTunnel relay server.
type Server struct {
	conn       *net.UDPConn
	clients    map[string]*Client // virtualIP string → Client
	addrMap    map[string]*Client // "ip:port" string → Client (O(1) lookup)
	mu         sync.RWMutex       // protects clients + addrMap
	subnet     *net.IPNet
	maxPlayers int
	serverIP   net.IP
	roomPass   string // room password (empty = no auth)

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

	return &Server{
		conn:        conn,
		clients:     make(map[string]*Client),
		addrMap:     make(map[string]*Client),
		subnet:      cfg.Subnet,
		maxPlayers:  cfg.MaxPlayers,
		serverIP:    serverIP,
		roomPass:    cfg.RoomPass,
		workers:     workers,
		pktCh:       make(chan pktJob, 4096),
		rateCount:   make(map[rateKey]int),
		maxPending:  cfg.MaxPlayers * 3,
		regCount:    make(map[string]int),
		maxRegPerIP: 5,
	}, nil
}

// Run starts the server and blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) {
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
		s.mu.Lock()
		now := time.Now()
		changed := false
		for ip, c := range s.clients {
			if now.Sub(c.LastSeen) > 45*time.Second {
				log.Printf("[-] %s (%s) 超时断开", c.Username, c.VirtualIP)
				delete(s.clients, ip)
				delete(s.addrMap, c.PublicAddr.String())
				changed = true
			}
		}
		// Clean up stale pending auth entries
		for addrStr, c := range s.addrMap {
			if c.auth == authChallengeSent && now.Sub(c.challengeAt) > 30*time.Second {
				delete(s.addrMap, addrStr)
				s.pendingAuth--
			}
		}
		s.mu.Unlock()

		if changed {
			s.sendPeerInfoTo(nil, nil, nil)
		}
	}
}

// ── Send Helpers ───────────────────────────────────────────────

func (s *Server) sendChecked(typ byte, payload []byte, to *net.UDPAddr) {
	data := protocol.EncodeChecked(typ, payload)
	s.conn.WriteToUDP(data, to)
}

func (s *Server) sendCheckedRaw(data []byte, to *net.UDPAddr) {
	s.conn.WriteToUDP(data, to)
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
