package server

import (
	"fmt"
	"log"
	"net"
	"time"

	"github.com/holipay/gametunnel/internal/auth"
	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/protocol"
)

// ── Auth Key ───────────────────────────────────────────────────

func (r *Room) getAuthKey(roomID string) ([]byte, error) {
	if v, ok := r.authKeys.Load(roomID); ok {
		return v.([]byte), nil
	}
	key := auth.DeriveKey(r.roomPass, roomID)
	if key != nil {
		r.authKeys.Store(roomID, key)
	}
	return key, nil
}

// ── Register ───────────────────────────────────────────────────

// cleanupPendingAuth removes the pending auth entry, decrements counters,
// and releases r.mu. MUST be called with r.mu held.
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
	r.mu.Unlock()
}

// checkRoomCapacityAndDuplicate checks if the room is full or the username is taken.
// Returns true if the client should be rejected. MUST be called with r.mu held.
// Releases r.mu and sends a kick packet before returning true.
func (r *Room) checkRoomCapacityAndDuplicate(username, roomID string, from *net.UDPAddr) bool {
	t := i18n.T()
	if len(r.clients) >= r.maxPlayers {
		r.decrementIPConnCount(addrToConnIPKey(from))
		r.mu.Unlock()
		r.sendKick(from, t.KickRoomFull)
		return true
	}
	for _, c := range r.clients {
		if c.auth == authChallengeSent {
			continue
		}
		if c.authRoomID == roomID && c.Username == username {
			r.decrementIPConnCount(addrToConnIPKey(from))
			r.mu.Unlock()
			r.sendKick(from, t.KickDuplicateName)
			return true
		}
	}
	return false
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

	if len(reg.Username) == 0 || len(reg.Username) > maxUsernameLen {
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

	if r.checkRoomCapacityAndDuplicate(reg.Username, reg.RoomID, from) {
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
		Username:   reg.Username,
		VirtualIP:  vip,
		PublicAddr: from,
		auth:       authNone,
		authRoomID: reg.RoomID,
	}
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
		Username:    reg.Username,
		PublicAddr:  from,
		auth:        authChallengeSent,
		challenge:   challenge,
		challengeAt: time.Now(),
		authRoomID:  reg.RoomID,
	}
	c.SetLastSeen(time.Now())
	r.addrMap[addrToRateKey(from)] = c
	r.pendingAuth++
	return challenge, true
}

// ── Auth Response ──────────────────────────────────────────────

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
		r.sendKick(from, t.KickAuthTimeout)
		return
	}

	authKey, err := r.getAuthKey(c.authRoomID)
	if err != nil || authKey == nil {
		r.cleanupPendingAuth(fromKey, oldKey, foundByScan, c)
		r.sendKick(from, t.KickInternalError)
		return
	}

	// Use the address from registration (c.PublicAddr) for HMAC verification,
	// NOT the auth response source address. NAT rebinding may have changed
	// the client's observed address between registration and auth response.
	if !auth.VerifyHMAC(authKey, resp.HMAC, c.challenge, resp.RoomID, resp.Username, c.PublicAddr) {
		r.cleanupPendingAuth(fromKey, oldKey, foundByScan, c)
		log.Printf(t.LogAuthFail, resp.Username, from)
		r.authFailures.Add(1)
		r.sendKickCode(from, protocol.KickCodeWrongPassword, t.KickWrongPassword)
		return
	}

	log.Printf(t.LogAuthPass, resp.Username, from)
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

	if r.checkRoomCapacityAndDuplicate(resp.Username, resp.RoomID, from) {
		return
	}

	reg := &protocol.RegisterPayload{RoomID: resp.RoomID, Username: resp.Username}
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
