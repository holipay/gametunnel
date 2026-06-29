package server

import (
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
func (r *Room) cleanupPendingAuth(fromKey, oldKey rateKey, foundByScan bool, c *Client) {
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
		return
	}

	t := i18n.T()

	// Version compatibility check
	if !protocol.IsCompatible(reg.Version, protocol.AppVersion) {
		r.sendKickCode(from, protocol.KickCodeVersionMismatch, fmt.Sprintf(t.KickVersionMismatch,
			reg.Version,
			protocol.AppVersion))
		return
	}

	if utf8.RuneCountInString(reg.Username) == 0 || utf8.RuneCountInString(reg.Username) > maxUsernameLen {
		r.sendKick(from, t.KickInvalidName)
		return
	}
	if len(reg.RoomID) == 0 || len(reg.RoomID) > maxRoomIDLen {
		r.sendKick(from, t.KickInvalidRoom)
		return
	}

	clientIP := addrToConnIPKey(from)
	if !r.checkRegRate(from) {
		r.sendKick(from, t.KickRateLimit)
		return
	}

	r.mu.Lock()
	fromKey := addrToRateKey(from)

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
	c.GenerateSessionToken()
	c.SetLastSeen(time.Now())
	r.clients[ipKey(vip)] = c
	r.addrMap[addrToRateKey(from)] = c

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
	r.addrMap[addrToRateKey(from)] = c
	r.pendingAuth++
	return challenge, true
}

// ── ECDH Key Exchange ─────────────────────────────────────────

func (r *Room) handleECDHConfirm(payload []byte, from *net.UDPAddr) {
	confirm, err := protocol.UnmarshalECDHConfirm(payload)
	if err != nil {
		return
	}

	t := i18n.T()
	r.mu.Lock()
	fromKey := addrToRateKey(from)
	c := r.addrMap[fromKey]

	if c == nil || !c.ecdhPending || c.ecdhPriv == nil {
		r.mu.Unlock()
		return
	}

	// Verify HMAC over both public keys (prevents MITM)
	authKey := r.getAuthKey(c.authRoomID)
	if authKey == nil {
		r.cleanupPendingAuth(fromKey, fromKey, false, c)
		r.mu.Unlock()
		r.sendKick(from, t.KickInternalError)
		return
	}

	if !auth.VerifyECDHMAC(authKey, confirm.HMAC[:], c.ecdhPub, confirm.PublicKey[:]) {
		log.Printf("[ecdh] HMAC verification failed for %s", c.Username)
		r.cleanupPendingAuth(fromKey, fromKey, false, c)
		r.mu.Unlock()
		r.sendKickCode(from, protocol.KickCodeWrongPassword, t.KickWrongPassword)
		return
	}

	// Compute shared secret
	shared, err := auth.ComputeECDHSharedSecret(c.ecdhPriv, confirm.PublicKey[:])
	if err != nil {
		log.Printf("[ecdh] shared secret computation failed: %v", err)
		r.cleanupPendingAuth(fromKey, fromKey, false, c)
		r.mu.Unlock()
		r.sendKick(from, t.KickInternalError)
		return
	}

	// Derive session key
	sessionKey := auth.DeriveSessionKey(shared, c.authRoomID)
	if sessionKey == nil {
		r.cleanupPendingAuth(fromKey, fromKey, false, c)
		r.mu.Unlock()
		r.sendKick(from, t.KickInternalError)
		return
	}

	// Store session key for later use (will be used to create cipher)
	// For now, clear ECDH state and proceed with registration
	c.ecdhPriv = nil
	c.ecdhPub = nil
	c.ecdhPending = false

	// Complete registration
	reg := &protocol.RegisterPayload{RoomID: c.authRoomID, Username: c.Username, Version: c.clientVersion}
	vip, ok := r.doRegisterClient(reg, from)
	if !ok {
		r.mu.Unlock()
		r.sendKick(from, t.KickIPExhausted)
		return
	}

	// Re-fetch the newly registered client — doRegisterClient replaces the
	// old pending-auth entry with a fresh Client in addrMap.
	c = r.addrMap[fromKey]
	c.SessionKey = sessionKey
	if r.pendingAuth > 0 {
		r.pendingAuth--
	}
	r.mu.Unlock()

	// Send AssignIP with ECDH flag
	assign := &protocol.AssignIPPayload{
		VirtualIP:  vip,
		SubnetMask: r.subnet.Mask,
		ServerIP:   r.serverIP,
		Version:    protocol.SetECDHFlag(protocol.AppVersion),
	}
	assign.SessionToken = c.SessionToken
	if data := assign.Marshal(); data != nil {
		r.sendChecked(protocol.TypeAssignIP, data, from)
	}
	r.sendPeerInfoToClient(from)
	r.invalidatePeerInfoCache()
	r.markDirty()
}

func (r *Room) handleAuthResponse(payload []byte, from *net.UDPAddr) {
	resp, err := protocol.UnmarshalAuthResponse(payload)
	if err != nil {
		return
	}
	if len(resp.HMAC) != auth.HMACSize {
		return
	}

	t := i18n.T()
	r.mu.Lock()
	fromKey := addrToRateKey(from)
	c := r.addrMap[fromKey]

	// If direct address lookup fails (NAT rebinding between register and
	// auth response), scan pending auth clients by username+roomID.
	var oldKey rateKey
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
		// If c == nil, the entry was already cleaned up (e.g. by CleanupStale
		// which already rolled back ipConnCount). Don't double-decrement.
		// Only rollback if c exists but has wrong auth state (genuine anomaly).
		// Use c.PublicAddr (original registration address) for decrement,
		// matching the increment in handleRegister.
		if c != nil {
			r.decrementIPConnCount(addrToConnIPKey(c.PublicAddr))
		}
		r.mu.Unlock()
		r.sendKick(from, t.KickAuthAbnormal)
		return
	}

	if time.Since(c.challengeAt) > 15*time.Second {
		r.cleanupPendingAuth(fromKey, oldKey, foundByScan, c)
		r.mu.Unlock()
		r.sendKick(from, t.KickAuthTimeout)
		return
	}

	authKey := r.getAuthKey(c.authRoomID)
	if authKey == nil {
		r.cleanupPendingAuth(fromKey, oldKey, foundByScan, c)
		r.mu.Unlock()
		r.sendKick(from, t.KickInternalError)
		return
	}

	// Use the address from registration (c.PublicAddr) for HMAC verification,
	// NOT the auth response source address. NAT rebinding may have changed
	// the client's observed address between registration and auth response.
	if !auth.VerifyHMAC(authKey, resp.HMAC, c.challenge, resp.RoomID, resp.Username, c.PublicAddr) {
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
		// Decrement for the ORIGINAL registration address (c.PublicAddr),
		// NOT the current `from`. NAT rebinding may have changed the
		// client's observed address between registration and auth response.
		if c.PublicAddr != nil {
			r.decrementIPConnCount(addrToConnIPKey(c.PublicAddr))
		}
		r.mu.Unlock()
		r.sendKick(from, t.KickRoomFull)
		return
	case checkDuplicate:
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

	// Initiate ECDH key exchange for forward secrecy (v1.7+ clients only).
	// Old clients don't understand ECDHExchange, so skip for them.
	if c.clientVersion >= protocol.MinTokenVersion {
		priv, pub, err := auth.GenerateECDHKeyPair()
		if err != nil {
			log.Printf("[ecdh] failed to generate keypair: %v", err)
			r.mu.Unlock()
			r.sendKick(from, t.KickInternalError)
			return
		}
		c.ecdhPriv = priv
		c.ecdhPub = pub
		c.ecdhPending = true
		r.addrMap[fromKey] = c
		r.pendingAuth++
		r.mu.Unlock()

		ecdhPkt := &protocol.ECDHExchangePayload{}
		copy(ecdhPkt.PublicKey[:], pub)
		r.sendChecked(protocol.TypeECDHExchange, ecdhPkt.Marshal(), from)
		return
	}

	// Old client (pre-v1.7): skip ECDH, register directly
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
}
