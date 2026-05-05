// GameTunnel Server — 公网中转服务器
//
// 职责：
//  1. 接受客户端注册（HMAC challenge-response 认证）
//  2. 中转游戏流量（CRC32 完整性校验）
//  3. 转发广播包（局域网游戏发现的关键）
//  4. 提供对等节点发现（NAT打洞）
//
// Usage:
//
//	server -addr :4700 -subnet 10.10.0.0/24 -max 10
//	server -addr :4700 -password myroomsecret
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/holipay/gametunnel/internal/netutil"
	"github.com/holipay/gametunnel/internal/protocol"
	"golang.org/x/sync/semaphore"
)

// Version is set at build time via -ldflags.
var Version = "dev"

// ── Auth State ─────────────────────────────────────────────────

type authState int

const (
	authNone      authState = iota // no password required, or already authenticated
	authChallengeSent              // challenge sent, waiting for response
)

// ── Client State ───────────────────────────────────────────────

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

type Server struct {
	conn       *net.UDPConn
	connMu     sync.Mutex // protects WriteToUDP
	clients    map[string]*Client // virtualIP string → Client
	addrMap    map[string]*Client // "ip:port" string → Client (O(1) lookup)
	mu         sync.RWMutex       // protects clients + addrMap
	subnet     *net.IPNet
	maxPlayers int
	serverIP   net.IP
	sem        *semaphore.Weighted
	roomPass   string // room password (empty = no auth)

	// Rate limiting: per-client packet count per window
	rateMu    sync.Mutex
	rateCount map[rateKey]int
	rateTick  *time.Ticker

	// Auth flood protection: limit pending (unauthenticated) connections
	pendingAuth int // count of entries in addrMap with auth == authChallengeSent
	maxPending  int // max allowed pending auth entries (0 = unlimited when no password)
}

// rateKey is a fixed-size key for rate limiting, avoiding string allocation per packet.
type rateKey struct {
IP   [4]byte
Port uint16
}

func addrToRateKey(addr *net.UDPAddr) rateKey {
	var k rateKey
	copy(k.IP[:], addr.IP.To4())
	k.Port = uint16(addr.Port)
	return k
}

func main() {
	addr := flag.String("addr", ":4700", "监听地址 (UDP)")
	subnetStr := flag.String("subnet", "10.10.0.0/24", "虚拟子网 (CIDR)")
	maxPlayers := flag.Int("max", 10, "最大玩家数")
	roomPass := flag.String("password", "", "房间密码（留空=无认证）")
	versionFlag := flag.Bool("version", false, "显示版本")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("gtunnel-server %s\n", Version)
		os.Exit(0)
	}

	_, subnet, err := net.ParseCIDR(*subnetStr)
	if err != nil {
		log.Fatalf("子网无效 %q: %v", *subnetStr, err)
	}

	serverIP := make(net.IP, 4)
	copy(serverIP, subnet.IP.To4())
	serverIP[3] = 1

	udpAddr, err := net.ResolveUDPAddr("udp4", *addr)
	if err != nil {
		log.Fatalf("解析地址失败: %v", err)
	}

	conn, err := net.ListenUDP("udp4", udpAddr)
	if err != nil {
		log.Fatalf("监听失败: %v", err)
	}

	// Note: auth key is derived per-client using the room ID from the register request.
	// roomPass is stored; if empty, no authentication is required.

	s := &Server{
		conn:       conn,
		clients:    make(map[string]*Client),
		addrMap:    make(map[string]*Client),
		subnet:     subnet,
		maxPlayers: *maxPlayers,
		serverIP:   serverIP,
		sem:        semaphore.NewWeighted(200),
		roomPass:   *roomPass,
		rateCount:  make(map[rateKey]int),
		maxPending: *maxPlayers * 3, // allow 3x max players for pending auth
	}

	// ── 优雅退出 ────────────────────────────────────────────
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		log.Printf("收到信号 %v，正在关闭...", sig)
		cancel()
		conn.Close()
	}()

	authStatus := "无认证"
	if *roomPass != "" {
		authStatus = "HMAC 认证 (密钥按房间ID派生)"
	}

	log.Println("╔═══════════════════════════════════════════╗")
	log.Println("║       GameTunnel Server 已启动            ║")
	log.Println("╠═══════════════════════════════════════════╣")
	log.Printf("║  监听:    %-31s ║", *addr)
	log.Printf("║  子网:    %-31s ║", subnet.String())
	log.Printf("║  服务器:  %-31s ║", serverIP)
	log.Printf("║  上限:    %-31d ║", *maxPlayers)
	log.Printf("║  认证:    %-31s ║", authStatus)
	log.Printf("║  版本:    %-31s ║", Version)
	log.Println("╚═══════════════════════════════════════════╝")

	go s.keepaliveLoop(ctx)
	go s.rateLimitLoop(ctx)

	buf := make([]byte, 65535)
	for {
		n, remoteAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				log.Println("Server 已关闭")
				return
			default:
				continue
			}
		}
		if n < 1 {
			continue
		}

		// Rate limit check
		if !s.checkRate(remoteAddr) {
			continue
		}

		pkt := make([]byte, n)
		copy(pkt, buf[:n])

		if !s.sem.TryAcquire(1) {
			continue
		}
		go func() {
			defer s.sem.Release(1)
			s.handlePacket(pkt, remoteAddr)
		}()
	}
}

// ── Rate Limiting ──────────────────────────────────────────────

const (
	rateLimit    = 500 // max packets per window per client
	rateInterval = time.Second
)

func (s *Server) checkRate(addr *net.UDPAddr) bool {
	key := addrToRateKey(addr)
	s.rateMu.Lock()
	s.rateCount[key]++
	ok := s.rateCount[key] <= rateLimit
	s.rateMu.Unlock()
	return ok
}

func (s *Server) rateLimitLoop(ctx context.Context) {
	s.rateTick = time.NewTicker(rateInterval)
	defer s.rateTick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.rateTick.C:
			s.rateMu.Lock()
			s.rateCount = make(map[rateKey]int)
			s.rateMu.Unlock()
		}
	}
}

// ── Packet Handling ────────────────────────────────────────────

func (s *Server) handlePacket(data []byte, from *net.UDPAddr) {
	// Verify CRC32 checksum
	msg, err := protocol.DecodeChecked(data)
	if err != nil {
		return // silently drop corrupt packets
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

// ── Registration (with optional HMAC auth) ─────────────────────

const (
	maxUsernameLen = 32
	maxRoomIDLen   = 32
)

func (s *Server) handleRegister(payload []byte, from *net.UDPAddr) {
	reg, err := protocol.UnmarshalRegister(payload)
	if err != nil {
		return
	}

	// Validate input lengths to prevent memory abuse
	if len(reg.Username) == 0 || len(reg.Username) > maxUsernameLen {
		s.sendKick(from, "用户名无效")
		return
	}
	if len(reg.RoomID) == 0 || len(reg.RoomID) > maxRoomIDLen {
		s.sendKick(from, "房间ID无效")
		return
	}

	s.mu.Lock()

	// Reconnect: same address already registered
	if existing := s.addrMap[from.String()]; existing != nil {
		existing.LastSeen = time.Now()
		selfIP := existing.VirtualIP
		s.mu.Unlock()
		s.sendAssignIP(selfIP, from)
		s.sendPeerInfoTo([]*net.UDPAddr{from}, nil, selfIP)
		return
	}

	// Capacity check
	if len(s.clients) >= s.maxPlayers {
		s.mu.Unlock()
		s.sendKick(from, "房间已满")
		return
	}

	// If no password required, register immediately.
	if s.roomPass == "" {
		s.registerClientLocked(reg, from)
		return
	}

	// Password required: check pending auth flood limit.
	if s.pendingAuth >= s.maxPending {
		s.mu.Unlock()
		s.sendKick(from, "服务器繁忙，请稍后重试")
		return
	}

	// Send challenge (lock released inside).
	s.sendAuthChallengeLocked(reg, from)
}

// registerClientLocked completes registration. MUST be called with s.mu held.
// Releases s.mu before returning.
func (s *Server) registerClientLocked(reg *protocol.RegisterPayload, from *net.UDPAddr) {
	vip := s.nextAvailableIP()
	if vip == nil {
		s.mu.Unlock()
		s.sendKick(from, "IP已耗尽")
		return
	}

	c := &Client{
		Username:   reg.Username,
		VirtualIP:  vip,
		PublicAddr: from,
		LastSeen:   time.Now(),
		auth:       authNone,
	}
	s.clients[vip.String()] = c
	s.addrMap[from.String()] = c
	log.Printf("[+] %s (%s) → %s  [在线: %d]",
		reg.Username, from, vip, len(s.clients))

	selfIP := vip
	s.mu.Unlock()

	s.sendAssignIP(selfIP, from)
	s.sendPeerInfoTo(nil, nil, selfIP)
}

// sendAuthChallengeLocked sends auth challenge. MUST be called with s.mu held.
// Releases s.mu before returning.
func (s *Server) sendAuthChallengeLocked(reg *protocol.RegisterPayload, from *net.UDPAddr) {
	challenge, err := protocol.GenerateChallenge()
	if err != nil {
		s.mu.Unlock()
		log.Printf("[auth] 生成 challenge 失败: %v", err)
		s.sendKick(from, "服务器内部错误")
		return
	}

	// Store pending auth state
	c := &Client{
		Username:    reg.Username,
		PublicAddr:  from,
		LastSeen:    time.Now(),
		auth:        authChallengeSent,
		challenge:   challenge,
		challengeAt: time.Now(),
		authRoomID:  reg.RoomID,
	}
	s.addrMap[from.String()] = c
	s.pendingAuth++
	s.mu.Unlock()

	acp := &protocol.AuthChallengePayload{Challenge: challenge}
	s.sendChecked(protocol.TypeAuthChallenge, acp.Marshal(), from)
}

func (s *Server) handleAuthResponse(payload []byte, from *net.UDPAddr) {
	resp, err := protocol.UnmarshalAuthResponse(payload)
	if err != nil {
		return
	}

	// Use full Lock (not RLock+RUnlock then Lock) to avoid race window.
	s.mu.Lock()
	c := s.addrMap[from.String()]

	if c == nil || c.auth != authChallengeSent {
		s.mu.Unlock()
		s.sendKick(from, "未请求认证")
		return
	}

	// Check challenge expiry (15 seconds)
	if time.Since(c.challengeAt) > 15*time.Second {
		delete(s.addrMap, from.String())
		s.pendingAuth--
		s.mu.Unlock()
		s.sendKick(from, "认证超时")
		return
	}

	// Derive auth key using the room ID from the original register request.
	authKey := protocol.DeriveKey(s.roomPass, c.authRoomID)
	if authKey == nil {
		delete(s.addrMap, from.String())
		s.pendingAuth--
		s.mu.Unlock()
		s.sendKick(from, "服务器内部错误")
		return
	}

	if !protocol.VerifyAuthHMAC(authKey, resp.HMAC, c.challenge, resp.RoomID, resp.Username, from) {
		delete(s.addrMap, from.String())
		s.pendingAuth--
		s.mu.Unlock()
		log.Printf("[auth] 认证失败: %s (%s)", resp.Username, from)
		s.sendKick(from, "密码错误")
		return
	}

	// Auth passed — complete registration
	log.Printf("[auth] 认证成功: %s (%s)", resp.Username, from)

	// Remove pending entry from addrMap
	delete(s.addrMap, from.String())
	s.pendingAuth--

	// Capacity check (might have changed)
	if len(s.clients) >= s.maxPlayers {
		s.mu.Unlock()
		s.sendKick(from, "房间已满")
		return
	}

	reg := &protocol.RegisterPayload{
		RoomID:   resp.RoomID,
		Username: resp.Username,
	}
	s.registerClientLocked(reg, from) // releases s.mu
}

// ── Keepalive ──────────────────────────────────────────────────

func (s *Server) handleKeepAlive(from *net.UDPAddr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.addrMap[from.String()]; c != nil {
		c.LastSeen = time.Now()
	}
}

// handleDisconnect removes a client that is gracefully disconnecting.
func (s *Server) handleDisconnect(from *net.UDPAddr) {
	s.mu.Lock()
	c := s.addrMap[from.String()]
	if c == nil {
		s.mu.Unlock()
		return
	}
	log.Printf("[-] %s (%s) 主动断开", c.Username, c.VirtualIP)
	delete(s.clients, c.VirtualIP.String())
	delete(s.addrMap, from.String())
	s.mu.Unlock()

	s.sendPeerInfoTo(nil, nil, nil)
}

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
		// Also clean up stale pending auth entries
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

// ── Peer Request ───────────────────────────────────────────────

func (s *Server) handlePeerRequest(from *net.UDPAddr) {
	s.mu.RLock()
	c := s.addrMap[from.String()]
	s.mu.RUnlock()

	if c == nil {
		return
	}

	s.sendPeerInfoTo([]*net.UDPAddr{from}, nil, c.VirtualIP)
}

// ── Relay (core) ───────────────────────────────────────────────

// handleRelay forwards a data packet. For unicast, it avoids re-encoding
// by directly wrapping the original payload. For broadcast, it must read
// the DstIP from the payload header to determine recipients.
func (s *Server) handleRelay(payload []byte, from *net.UDPAddr) {
	// Verify sender is registered
	s.mu.RLock()
	sender := s.addrMap[from.String()]
	s.mu.RUnlock()
	if sender == nil {
		return // drop packets from unauthenticated clients
	}

	// payload is DataPayload: [4B srcIP][4B dstIP][data...]
	if len(payload) < 8 {
		return
	}

	srcIP := net.IP(payload[0:4])
	dstIP := net.IP(payload[4:8])

	// Validate srcIP matches the sender's virtual IP (anti-spoofing)
	if !srcIP.Equal(sender.VirtualIP) {
		return // drop spoofed packets
	}

	encoded := protocol.EncodeChecked(protocol.TypeData, payload) // re-wrap as-is, no decode

	// Broadcast
	if netutil.IsBroadcast(dstIP, s.subnet) {
		s.mu.RLock()
		targets := make([]*net.UDPAddr, 0, len(s.clients))
		for _, c := range s.clients {
			if c.PublicAddr.String() != from.String() {
				targets = append(targets, c.PublicAddr)
			}
		}
		s.mu.RUnlock()

		for _, addr := range targets {
			s.sendCheckedRaw(encoded, addr)
		}
		return
	}

	// Unicast
	s.mu.RLock()
	dst, ok := s.clients[dstIP.String()]
	s.mu.RUnlock()

	if !ok {
		return
	}
	s.sendCheckedRaw(encoded, dst.PublicAddr)
}

// ── NAT Hole Punch ─────────────────────────────────────────────

func (s *Server) handleHolePunch(payload []byte, from *net.UDPAddr) {
	if len(payload) < 4 {
		return
	}
	dstIP := net.IP(payload[:4])

	srcIP4 := from.IP.To4()
	if srcIP4 == nil {
		return
	}

	s.mu.RLock()
	dst, ok := s.clients[dstIP.String()]
	s.mu.RUnlock()

	if !ok {
		return
	}

	addrStr := from.String()
	punchData := make([]byte, 4+len(addrStr))
	copy(punchData[:4], srcIP4)
	copy(punchData[4:], []byte(addrStr))
	s.sendChecked(protocol.TypeHolePunch, punchData, dst.PublicAddr)
}

// ── Peer Info Broadcast ────────────────────────────────────────

func (s *Server) sendPeerInfoTo(targets []*net.UDPAddr, exclude *net.UDPAddr, selfIP net.IP) {
	s.mu.RLock()
	snapshot := make([]peerSnapshot, 0, len(s.clients))
	for _, c := range s.clients {
		snapshot = append(snapshot, peerSnapshot{
			virtualIP:  c.VirtualIP,
			publicAddr: c.PublicAddr,
			username:   c.Username,
		})
	}
	s.mu.RUnlock()

	peers := &protocol.PeerInfoPayload{}
	for _, sn := range snapshot {
		if selfIP != nil && sn.virtualIP.Equal(selfIP) {
			continue
		}
		peers.Peers = append(peers.Peers, protocol.PeerInfoEntry{
			VirtualIP:  sn.virtualIP,
			PublicAddr: sn.publicAddr,
			Username:   sn.username,
		})
	}

	encoded := protocol.EncodeChecked(protocol.TypePeerInfo, peers.Marshal())

	if targets != nil {
		for _, addr := range targets {
			s.sendCheckedRaw(encoded, addr)
		}
		return
	}

	for _, sn := range snapshot {
		if exclude != nil && sn.publicAddr.String() == exclude.String() {
			continue
		}
		s.sendCheckedRaw(encoded, sn.publicAddr)
	}
}

// ── IP Allocation ──────────────────────────────────────────────

func (s *Server) nextAvailableIP() net.IP {
	base := s.subnet.IP.To4()
	for octet := 2; octet < 255; octet++ {
		candidate := net.IPv4(base[0], base[1], base[2], byte(octet))
		if candidate.Equal(s.serverIP) {
			continue
		}
		if _, taken := s.clients[candidate.String()]; !taken {
			return candidate
		}
	}
	return nil
}

// ── Send Helpers (thread-safe, with checksum) ──────────────────

// sendChecked encodes a message with CRC32 and sends it.
func (s *Server) sendChecked(typ byte, payload []byte, to *net.UDPAddr) {
	data := protocol.EncodeChecked(typ, payload)
	s.sendCheckedRaw(data, to)
}

// sendCheckedRaw sends an already-encoded packet (with checksum).
func (s *Server) sendCheckedRaw(data []byte, to *net.UDPAddr) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	s.conn.WriteToUDP(data, to)
}

func (s *Server) sendAssignIP(vip net.IP, to *net.UDPAddr) {
	assign := &protocol.AssignIPPayload{
		VirtualIP:  vip,
		SubnetMask: s.subnet.Mask,
		ServerIP:   s.serverIP,
	}
	s.sendChecked(protocol.TypeAssignIP, assign.Marshal(), to)
}

func (s *Server) sendKick(to *net.UDPAddr, reason string) {
	kick := &protocol.KickPayload{Reason: reason}
	s.sendChecked(protocol.TypeKick, kick.Marshal(), to)
}

// ── Types ──────────────────────────────────────────────────────

type peerSnapshot struct {
	virtualIP  net.IP
	publicAddr *net.UDPAddr
	username   string
}
