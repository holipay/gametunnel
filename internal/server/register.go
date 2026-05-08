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
		s.sendKick(from, "用户名不合法")
		return
	}
	if len(reg.RoomID) == 0 || len(reg.RoomID) > maxRoomIDLen {
		s.sendKick(from, "房间ID不合法")
		return
	}

	// Per-IP registration rate limit
	clientIP := from.IP.String()
	if !s.checkRegRate(clientIP) {
		s.sendKick(from, "注册过于频繁，请稍后再试")
		return
	}

	s.mu.Lock()
	fromKey := addrToRateKey(from)

	// Auth in progress: same address already registered and pending auth
	if existing := s.addrMap[fromKey]; existing != nil && existing.auth == authChallengeSent {
		s.mu.Unlock()
		s.sendKick(from, "认证进行中，请等待")
		return
	}

	// Reconnect: same address already registered and authenticated
	if existing := s.addrMap[fromKey]; existing != nil {
		existing.LastSeen = time.Now()
		selfIP := existing.VirtualIP
		s.mu.Unlock()
		s.sendAssignIP(selfIP, from)
		s.sendPeerInfoToClient(from)
		return
	}

	// Capacity check
	if len(s.clients) >= s.maxPlayers {
		s.mu.Unlock()
		s.sendKick(from, "房间已满")
		return
	}

	// ====== Duplicate username detection ======
	// Reject if any authenticated client in the same room already uses this name.
	// This prevents confusion when a user accidentally launches a second instance
	// that somehow bypasses the local single-instance lock (e.g., different binary path).
	for _, c := range s.clients {
		if c.auth == authNone || c.auth == authChallengeSent {
			continue // skip unauthenticated/pending entries
		}
		if c.authRoomID == reg.RoomID && c.Username == reg.Username {
			s.mu.Unlock()
			s.sendKick(from, "同房间内已存在相同用户名的玩家，请更换用户名")
			return
		}
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
		s.sendKick(from, "IP已分配完")
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
	// Send full peer list to new player immediately (for hole punching)
	s.sendPeerInfoToClient(from)
	// Mark dirty so existing players learn about the new player within peerInfoInterval
	s.peerInfoDirty.Store(true)
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
		s.sendKick(from, "认证状态异常")
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

	// Get cached auth key for this roomID
	authKey := s.getAuthKey(c.authRoomID)
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
	log.Printf("[auth] 认证通过: %s (%s)", resp.Username, from)

	delete(s.addrMap, fromKey)
	s.pendingAuth--

	if len(s.clients) >= s.maxPlayers {
		s.mu.Unlock()
		s.sendKick(from, "房间已满")
		return
	}

	// ====== Duplicate username detection (post-auth) ======
	for _, existing := range s.clients {
		if existing.auth == authNone || existing.auth == authChallengeSent {
			continue
		}
		if existing.authRoomID == resp.RoomID && existing.Username == resp.Username {
			s.mu.Unlock()
			s.sendKick(from, "同房间内已存在相同用户名的玩家，请更换用户名")
			return
		}
	}

	reg := &protocol.RegisterPayload{
		RoomID:   resp.RoomID,
		Username: resp.Username,
	}
	s.registerClientLocked(reg, from)
}
