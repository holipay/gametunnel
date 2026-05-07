package server

import (
	"log"
	"net"
	"time"

	"github.com/holipay/gametunnel/internal/auth"
	"github.com/holipay/gametunnel/internal/protocol"
)

const (
	maxUsernameLen = 32
	maxRoomIDLen   = 32
)

// handleRegister processes a client registration request.
func (s *Server) handleRegister(payload []byte, from *net.UDPAddr) {
	reg, err := protocol.UnmarshalRegister(payload)
	if err != nil {
		return
	}

	// Validate input lengths
	if len(reg.Username) == 0 || len(reg.Username) > maxUsernameLen {
		s.sendKick(from, "用户名无效")
		return
	}
	if len(reg.RoomID) == 0 || len(reg.RoomID) > maxRoomIDLen {
		s.sendKick(from, "房间ID无效")
		return
	}

	// Per-IP registration rate limit
	clientIP := from.IP.String()
	if !s.checkRegRate(clientIP) {
		s.sendKick(from, "注册过于频繁，请稍后重试")
		return
	}

	s.mu.Lock()

	// Reconnect: same address already registered
	fromKey := addrToRateKey(from)
	if existing := s.addrMap[fromKey]; existing != nil {
		existing.LastSeen = time.Now()
		selfIP := existing.VirtualIP
		s.mu.Unlock()
		s.sendAssignIP(selfIP, from)
		s.sendPeerInfoTo([]*net.UDPAddr{from}, selfIP)
		return
	}

	// Capacity check
	if len(s.clients) >= s.maxPlayers {
		s.mu.Unlock()
		s.sendKick(from, "房间已满")
		return
	}

	// No password required — register immediately
	if s.roomPass == "" {
		s.registerClientLocked(reg, from)
		return
	}

	// Password required: check pending auth flood limit
	if s.pendingAuth >= s.maxPending {
		s.mu.Unlock()
		s.sendKick(from, "服务器繁忙，请稍后重试")
		return
	}

	// Send challenge
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

	s.markIPUsed(vip)

	c := &Client{
		Username:   reg.Username,
		VirtualIP:  vip,
		PublicAddr: from,
		LastSeen:   time.Now(),
		auth:       authNone,
	}
	s.clients[ip4Key(vip)] = c
	s.addrMap[addrToRateKey(from)] = c
	log.Printf("[+] %s (%s) → %s  [在线: %d]",
		reg.Username, from, vip, len(s.clients))

	selfIP := vip
	s.mu.Unlock()

	s.sendAssignIP(selfIP, from)
	s.sendPeerInfoTo(nil, selfIP)
}

// sendAuthChallengeLocked sends auth challenge. MUST be called with s.mu held.
// Releases s.mu before returning.
func (s *Server) sendAuthChallengeLocked(reg *protocol.RegisterPayload, from *net.UDPAddr) {
	challenge, err := auth.GenerateChallenge()
	if err != nil {
		s.mu.Unlock()
		log.Printf("[auth] 生成 challenge 失败: %v", err)
		s.sendKick(from, "服务器内部错误")
		return
	}

	c := &Client{
		Username:    reg.Username,
		PublicAddr:  from,
		LastSeen:    time.Now(),
		auth:        authChallengeSent,
		challenge:   challenge,
		challengeAt: time.Now(),
		authRoomID:  reg.RoomID,
	}
	s.addrMap[addrToRateKey(from)] = c
	s.pendingAuth++
	s.mu.Unlock()

	acp := &protocol.AuthChallengePayload{Challenge: challenge, ClientAddr: from.String()}
	s.sendChecked(protocol.TypeAuthChallenge, acp.Marshal(), from)
}

// handleAuthResponse processes a client's HMAC auth response.
func (s *Server) handleAuthResponse(payload []byte, from *net.UDPAddr) {
	resp, err := protocol.UnmarshalAuthResponse(payload)
	if err != nil {
		return
	}

	if len(resp.HMAC) != auth.HMACSize {
		return
	}

	s.mu.Lock()
	fromKey := addrToRateKey(from)
	c := s.addrMap[fromKey]

	if c == nil || c.auth != authChallengeSent {
		s.mu.Unlock()
		s.sendKick(from, "未请求认证")
		return
	}

	// Check challenge expiry (15 seconds)
	if time.Since(c.challengeAt) > 15*time.Second {
		delete(s.addrMap, fromKey)
		s.pendingAuth--
		s.mu.Unlock()
		s.sendKick(from, "认证超时")
		return
	}

	// Derive auth key using the room ID from the original register request
	authKey := auth.DeriveKey(s.roomPass, c.authRoomID)
	if authKey == nil {
		delete(s.addrMap, fromKey)
		s.pendingAuth--
		s.mu.Unlock()
		s.sendKick(from, "服务器内部错误")
		return
	}

	if !auth.VerifyHMAC(authKey, resp.HMAC, c.challenge, resp.RoomID, resp.Username, from) {
		delete(s.addrMap, fromKey)
		s.pendingAuth--
		s.mu.Unlock()
		log.Printf("[auth] 认证失败: %s (%s)", resp.Username, from)
		s.sendKick(from, "密码错误")
		return
	}

	// Auth passed — complete registration
	log.Printf("[auth] 认证成功: %s (%s)", resp.Username, from)

	delete(s.addrMap, fromKey)
	s.pendingAuth--

	if len(s.clients) >= s.maxPlayers {
		s.mu.Unlock()
		s.sendKick(from, "房间已满")
		return
	}

	reg := &protocol.RegisterPayload{
		RoomID:   resp.RoomID,
		Username: resp.Username,
	}
	s.registerClientLocked(reg, from)
}
