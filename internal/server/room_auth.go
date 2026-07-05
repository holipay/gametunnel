package server

import (
	"github.com/holipay/gametunnel/internal/netkey"
	"fmt"
	"log"
	"net"
	"time"
	"unicode/utf8"

	"github.com/holipay/gametunnel/internal/auth"
	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/protocol"
)

// ── Auth Key ───────────────────────────────────────────────────

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

// ── Register ───────────────────────────────────────────────────

// cleanupPendingAuth removes the pending auth entry and decrements counters.
// MUST be called with r.mu held. The caller must release r.mu after calling.
// It decrements ipConnCount for the original registration address (c.PublicAddr),
// NOT the current `from` address. When NAT rebinding occurs, `from` may differ
// from the registration address — decrementing `from` would leave the original
// address's count permanently incremented (memory leak) and the current address's
// count negative (blocking future registrations from that IP).
func (r *Room) cleanupPendingAuth(fromKey, oldKey netkey.RateKey, foundByScan bool, c *Client) {
	deleteKey := fromKey
	if foundByScan {
		deleteKey = oldKey
	}
	delete(r.addrMap, deleteKey)
	if r.pendingAuth > 0 {
		r.pendingAuth--
	}
	// Decrement for the ORIGINAL registration address, not the current source.
	if c.PublicAddr != nil {
		r.decrementIPConnCount(addrToConnIPKey(c.PublicAddr))
	}
}

type checkResult int

const (
	checkOK          checkResult = iota
	checkRoomFull
	checkDuplicate
)

// checkCapacityAndDuplicate checks if the room is full or the username is taken.
// MUST be called with r.mu held. The caller must release mu and send the kick.
func (r *Room) checkCapacityAndDuplicate(username, roomID string) checkResult {
	if len(r.clients) >= r.maxPlayers {
		return checkRoomFull
	}
	for _, c := range r.clients {
		if c.auth == authChallengeSent {
			continue
		}
		if c.authRoomID == roomID && c.Username == username {
			return checkDuplicate
		}
	}
	return checkOK
}

func (r *Room) handleRegister(payload []byte, from *net.UDPAddr) {
	reg, err := protocol.UnmarshalRegister(payload)
	if err != nil {
		log.Printf("[register] failed to parse from %s: %v", from, err)
		return
	}

	t := i18n.T()
	log.Printf("[register] request from %s: user=%s room=%s ver=%d", from, reg.Username, reg.RoomID, reg.Version)

	// Version compatibility check
	if !protocol.IsCompatible(reg.Version, protocol.AppVersion) {
		log.Printf("[register] version mismatch from %s: client=%d server=%d", from, reg.Version, protocol.AppVersion)
		r.sendKickCode(from, protocol.KickCodeVersionMismatch, fmt.Sprintf(t.KickVersionMismatch,
			reg.Version,
			protocol.AppVersion))
		return
	}

	if utf8.RuneCountInString(reg.Username) == 0 || utf8.RuneCountInString(reg.Username) > maxUsernameLen {
		log.Printf("[register] invalid username %q from %s", reg.Username, from)
		r.sendKick(from, t.KickInvalidName)
		return
	}
	if len(reg.RoomID) == 0 || len(reg.RoomID) > maxRoomIDLen {
		log.Printf("[register] invalid roomID %q from %s", reg.RoomID, from)
		r.sendKick(from, t.KickInvalidRoom)
		return
	}

	clientIP := addrToConnIPKey(from)
	if !r.checkRegRate(from) {
		log.Printf("[register] rate limited: %s (user=%s)", from, reg.Username)
		r.sendKick(from, t.KickRateLimit)
		return
	}

	r.mu.Lock()
	fromKey := netkey.AddrToRateKey(from)

	if existing := r.addrMap[fromKey]; existing != nil && existing.auth == authChallengeSent {
		// Clean up stale auth entry so the client can retry immediately
		// instead of being blocked for 30s until keepaliveLoop cleans it up.
		// Also roll back the IP count from the previous registration attempt.
		if existing.PublicAddr != nil {
			r.decrementIPConnCount(addrToConnIPKey(existing.PublicAddr))
		}
		delete(r.addrMap, fromKey)
		if r.pendingAuth > 0 {
			r.pendingAuth--
		}
		// Fall through to allow new registration
	}

	if existing := r.addrMap[fromKey]; existing != nil {
		existing.SetLastSeen(time.Now())
		existing.clientVersion = reg.Version
		selfIP := existing.VirtualIP
		r.mu.Unlock()
		r.sendAssignIP(selfIP, from)
		r.sendPeerInfoToClient(from)
		return
	}

	// Try to restore a previously persisted client (state persistence).
	// If a placeholder exists with matching username, attach the real address.
	if restored := r.resolveRestoredClient(reg.Username, reg.RoomID, from); restored != nil {
		selfIP := restored.VirtualIP
		r.mu.Unlock()
		r.sendAssignIP(selfIP, from)
		r.sendPeerInfoToClient(from)
		r.invalidatePeerInfoCache()
		r.markDirty()
		go r.sendPeerInfoBroadcast()
		return
	}

	r.ipConnMu.Lock()
	ipCount := r.ipConnCount[clientIP]
	if ipCount >= r.maxPerIP {
		r.ipConnMu.Unlock()
		r.mu.Unlock()
		r.sendKick(from, t.KickIPLimit)
		return
	}
	// Increment immediately under the same lock to prevent TOCTOU race
	r.ipConnCount[clientIP]++
	r.ipConnMu.Unlock()

	switch r.checkCapacityAndDuplicate(reg.Username, reg.RoomID) {
	case checkRoomFull:
		r.decrementIPConnCount(clientIP)
		r.mu.Unlock()
		r.sendKick(from, t.KickRoomFull)
		return
	case checkDuplicate:
		r.decrementIPConnCount(clientIP)
		r.mu.Unlock()
		r.sendKick(from, t.KickDuplicateName)
		return
	}

	if r.roomPass == "" {
		vip, ok := r.doRegisterClient(reg, from)
		r.mu.Unlock()
		if !ok {
			r.sendKick(from, t.KickIPExhausted)
			return
		}
		r.sendAssignIP(vip, from)
		r.sendPeerInfoToClient(from)
		r.invalidatePeerInfoCache()
		r.markDirty()
		go r.sendPeerInfoBroadcast()
		return
	}

	if r.pendingAuth >= r.maxPending {
		r.decrementIPConnCount(clientIP)
		r.mu.Unlock()
		r.sendKick(from, t.KickServerBusy)
		return
	}

	challenge, ok := r.doSendAuthChallenge(reg, from)
	if !ok {
		r.decrementIPConnCount(clientIP)
		r.mu.Unlock()
		r.sendKick(from, t.KickInternalError)
		return
	}
	r.mu.Unlock()

	acp := &protocol.AuthChallengePayload{Challenge: challenge, ClientAddr: from.String()}
	r.sendChecked(protocol.TypeAuthChallenge, acp.Marshal(), from)
}

// doRegisterClient creates a client entry under the held mu lock.
// Returns the assigned virtual IP and true on success, or nil and false
// on failure (e.g. IP exhaustion). The caller MUST release mu after return.
func (r *Room) doRegisterClient(reg *protocol.RegisterPayload, from *net.UDPAddr) (net.IP, bool) {
	t := i18n.T()
	vip := r.nextAvailableIP()
	if vip == nil {
		return nil, false
	}

	r.markIPUsed(vip)

	c := &Client{
		Username:      reg.Username,
		VirtualIP:     vip,
		PublicAddr:    from,
		auth:          authNone,
		authRoomID:    reg.RoomID,
		clientVersion: reg.Version,
	}
	if err := c.GenerateSessionToken(); err != nil {
		r.markIPFree(vip)
		log.Printf("register: %v", err)
		return nil, false
	}
	c.SetLastSeen(time.Now())
	r.clients[netkey.IPKey(vip)] = c
	r.addrMap[netkey.AddrToRateKey(from)] = c

	log.Printf(t.LogPlayerJoin, reg.Username, from, vip, len(r.clients))

	r.lastActivity.Store(time.Now().UnixNano())
	r.totalRegistrations.Add(1)
	if cur := uint32(len(r.clients)); cur > r.peakPlayers.Load() {
		r.peakPlayers.Store(cur)
	}

	return vip, true
}

// doSendAuthChallenge generates and stores a challenge under the held mu lock.
// Returns the challenge bytes and true on success. The caller MUST release mu.
func (r *Room) doSendAuthChallenge(reg *protocol.RegisterPayload, from *net.UDPAddr) ([]byte, bool) {
	challenge, err := auth.GenerateChallenge()
	if err != nil {
		log.Printf(i18n.T().LogChallengeFail, err)
		return nil, false
	}

	c := &Client{
		Username:      reg.Username,
		PublicAddr:    from,
		auth:          authChallengeSent,
		challenge:     challenge,
		challengeAt:   time.Now(),
		authRoomID:    reg.RoomID,
		clientVersion: reg.Version,
	}
	c.SetLastSeen(time.Now())
	r.addrMap[netkey.AddrToRateKey(from)] = c
	r.pendingAuth++
	return challenge, true
}

func (r *Room) handleAuthResponse(payload []byte, from *net.UDPAddr) {
	resp, err := protocol.UnmarshalAuthResponse(payload)
	if err != nil {
		log.Printf("[auth] failed to parse AuthResponse from %s: %v", from, err)
		return
	}
	if len(resp.HMAC) != auth.HMACSize {
		log.Printf("[auth] invalid HMAC length from %s: got %d, want %d", from, len(resp.HMAC), auth.HMACSize)
		return
	}
	log.Printf("[auth] received AuthResponse from %s: user=%s room=%s", from, resp.Username, resp.RoomID)

	t := i18n.T()
	r.mu.Lock()
	fromKey := netkey.AddrToRateKey(from)
	c := r.addrMap[fromKey]

	// If direct address lookup fails (NAT rebinding between register and
	// auth response), scan pending auth clients by username+roomID.
	var oldKey netkey.RateKey
	var foundByScan bool
	if c == nil || c.auth != authChallengeSent {
		for key, client := range r.addrMap {
			if client.auth == authChallengeSent &&
				client.authRoomID == resp.RoomID &&
				client.Username == resp.Username {
				c = client
				oldKey = key
				foundByScan = true
				break
			}
		}
	}

	if c == nil || c.auth != authChallengeSent {
		if c == nil {
			log.Printf("[auth] client not found in addrMap for %s (user=%s room=%s), scan=%v", from, resp.Username, resp.RoomID, foundByScan)
		} else {
			log.Printf("[auth] auth state mismatch for %s: got %d, want %d (user=%s)", from, c.auth, authChallengeSent, c.Username)
			// Clean up pending auth entry to prevent addrMap and pendingAuth counter leak
			r.cleanupPendingAuth(fromKey, oldKey, foundByScan, c)
		}
		r.mu.Unlock()
		r.sendKick(from, t.KickAuthAbnormal)
		return
	}

	if time.Since(c.challengeAt) > 15*time.Second {
		log.Printf("[auth] challenge expired for %s: age=%v (user=%s)", from, time.Since(c.challengeAt), c.Username)
		r.cleanupPendingAuth(fromKey, oldKey, foundByScan, c)
		r.mu.Unlock()
		r.sendKick(from, t.KickAuthTimeout)
		return
	}

	authKey := r.getAuthKey(c.authRoomID)
	if authKey == nil {
		log.Printf("[auth] authKey is nil for roomID=%s (user=%s from=%s)", c.authRoomID, c.Username, from)
		r.cleanupPendingAuth(fromKey, oldKey, foundByScan, c)
		r.mu.Unlock()
		r.sendKick(from, t.KickInternalError)
		return
	}

	// Use the address from registration (c.PublicAddr) for HMAC verification,
	// NOT the auth response source address. NAT rebinding may have changed
	// the client's observed address between registration and auth response.
	if !auth.VerifyHMAC(authKey, resp.HMAC, c.challenge, resp.RoomID, resp.Username, c.PublicAddr) {
		log.Printf("[auth] HMAC verification FAILED for %s: user=%s room=%s regAddr=%s fromAddr=%s",
			from, resp.Username, resp.RoomID, c.PublicAddr, from)
		r.cleanupPendingAuth(fromKey, oldKey, foundByScan, c)
		r.mu.Unlock()
		log.Printf(t.LogAuthFail, resp.Username, from)
		r.authFailures.Add(1)
		r.sendKickCode(from, protocol.KickCodeWrongPassword, t.KickWrongPassword)
		return
	}

	log.Printf(t.LogAuthPass, resp.Username, from)

	// Check room capacity BEFORE mutating addrMap so a rejection
	// doesn't leave state partially modified.
	switch r.checkCapacityAndDuplicate(resp.Username, resp.RoomID) {
	case checkRoomFull:
		log.Printf("[auth] room full, rejecting %s from %s", resp.Username, from)
		if c.PublicAddr != nil {
			r.decrementIPConnCount(addrToConnIPKey(c.PublicAddr))
		}
		r.mu.Unlock()
		r.sendKick(from, t.KickRoomFull)
		return
	case checkDuplicate:
		log.Printf("[auth] duplicate name %s from %s", resp.Username, from)
		if c.PublicAddr != nil {
			r.decrementIPConnCount(addrToConnIPKey(c.PublicAddr))
		}
		r.mu.Unlock()
		r.sendKick(from, t.KickDuplicateName)
		return
	}

	deleteKey := fromKey
	if foundByScan {
		deleteKey = oldKey
	}
	delete(r.addrMap, deleteKey)
	if r.pendingAuth > 0 {
		r.pendingAuth--
	}

	// Update address if NAT rebinding occurred
	if foundByScan {
		c.PublicAddr = from
		r.addrMap[fromKey] = c
	}

	// Register directly
	reg := &protocol.RegisterPayload{RoomID: c.authRoomID, Username: c.Username, Version: c.clientVersion}
	vip, ok := r.doRegisterClient(reg, from)
	r.mu.Unlock()
	if !ok {
		r.sendKick(from, t.KickIPExhausted)
		return
	}
	r.sendAssignIP(vip, from)
	r.sendPeerInfoToClient(from)
	r.invalidatePeerInfoCache()
	r.markDirty()
	go r.sendPeerInfoBroadcast()
}
