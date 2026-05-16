package server

import (
	"log"
	"net"
	"time"

	"github.com/holipay/gametunnel/internal/i18n"
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

	t := i18n.T()

	// Validate input lengths
	if len(reg.Username) == 0 || len(reg.Username) > maxUsernameLen {
		s.sendKick(from, t.KickInvalidName)
		return
	}
	if len(reg.RoomID) == 0 || len(reg.RoomID) > maxRoomIDLen {
		s.sendKick(from, t.KickInvalidRoom)
		return
	}

	// Per-IP registration rate limit
	clientIP := from.IP.String()
	if !s.checkRegRate(clientIP) {
		s.sendKick(from, t.KickRateLimit)
		return
	}

	s.mu.Lock()
	fromKey := addrToRateKey(from)

	// Auth in progress: same address already registered and pending auth
	if existing := s.addrMap[fromKey]; existing != nil && existing.auth == authChallengeSent {
		s.mu.Unlock()
		s.sendKick(from, t.KickAuthPending)
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

	// Per-IP connection count limit
	s.ipConnMu.Lock()
	ipCount := s.ipConnCount[clientIP]
	s.ipConnMu.Unlock()
	if ipCount >= s.maxPerIP {
		s.mu.Unlock()
		s.sendKick(from, t.KickIPLimit)
		return
	}

	// Capacity check
	if len(s.clients) >= s.maxPlayers {
		s.mu.Unlock()
		s.sendKick(from, t.KickRoomFull)
		return
	}

	// ====== Duplicate username detection ======
	for _, c := range s.clients {
		if c.auth == authNone || c.auth == authChallengeSent {
			continue
		}
		if c.authRoomID == reg.RoomID && c.Username == reg.Username {
			s.mu.Unlock()
			s.sendKick(from, t.KickDuplicateName)
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
		s.sendKick(from, t.KickServerBusy)
		return
	}

	// Send challenge
	s.sendAuthChallengeLocked(reg, from)
}

// registerClientLocked completes registration. MUST be called with s.mu held.
// Releases s.mu before returning.
func (s *Server) registerClientLocked(reg *protocol.RegisterPayload, from *net.UDPAddr) {
	t := i18n.T()
	vip := s.nextAvailableIP()
	if vip == nil {
		s.mu.Unlock()
		s.sendKick(from, t.KickIPExhausted)
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

	// Track per-IP connection count
	clientIP := from.IP.String()
	s.ipConnMu.Lock()
	s.ipConnCount[clientIP]++
	s.ipConnMu.Unlock()

	log.Printf(t.LogPlayerJoin, reg.Username, from, vip, len(s.clients))

	selfIP := vip
	s.mu.Unlock()

	s.sendAssignIP(selfIP, from)
	s.sendPeerInfoToClient(from)
	s.peerInfoDirty.Store(true)
}

// sendAuthChallengeLocked sends auth challenge. MUST be called with s.mu held.
// Releases s.mu before returning.
func (s *Server) sendAuthChallengeLocked(reg *protocol.RegisterPayload, from *net.UDPAddr) {
	t := i18n.T()
	challenge, err := auth.GenerateChallenge()
	if err != nil {
		s.mu.Unlock()
		log.Printf(t.LogChallengeFail, err)
		s.sendKick(from, t.KickInternalError)
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

	t := i18n.T()

	s.mu.Lock()
	fromKey := addrToRateKey(from)
	c := s.addrMap[fromKey]

	if c == nil || c.auth != authChallengeSent {
		s.mu.Unlock()
		s.sendKick(from, t.KickAuthAbnormal)
		return
	}

	// Check challenge expiry (15 seconds)
	if time.Since(c.challengeAt) > 15*time.Second {
		delete(s.addrMap, fromKey)
		s.pendingAuth--
		s.mu.Unlock()
		s.sendKick(from, t.KickAuthTimeout)
		return
	}

	// Get cached auth key for this roomID
	authKey := s.getAuthKey(c.authRoomID)
	if authKey == nil {
		delete(s.addrMap, fromKey)
		s.pendingAuth--
		s.mu.Unlock()
		s.sendKick(from, t.KickInternalError)
		return
	}

	if !auth.VerifyHMAC(authKey, resp.HMAC, c.challenge, resp.RoomID, resp.Username, from) {
		delete(s.addrMap, fromKey)
		s.pendingAuth--
		s.mu.Unlock()
		log.Printf(t.LogAuthFail, resp.Username, from)
		s.sendKick(from, t.KickWrongPassword)
		return
	}

	// Auth passed — complete registration
	log.Printf(t.LogAuthPass, resp.Username, from)

	delete(s.addrMap, fromKey)
	s.pendingAuth--

	if len(s.clients) >= s.maxPlayers {
		s.mu.Unlock()
		s.sendKick(from, t.KickRoomFull)
		return
	}

	// ====== Duplicate username detection (post-auth) ======
	for _, existing := range s.clients {
		if existing.auth == authNone || existing.auth == authChallengeSent {
			continue
		}
		if existing.authRoomID == resp.RoomID && existing.Username == resp.Username {
			s.mu.Unlock()
			s.sendKick(from, t.KickDuplicateName)
			return
		}
	}

	reg := &protocol.RegisterPayload{
		RoomID:   resp.RoomID,
		Username: resp.Username,
	}
	s.registerClientLocked(reg, from)
}
