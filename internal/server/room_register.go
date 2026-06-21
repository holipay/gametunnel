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

// ── Register ───────────────────────────────────────────────────

func (r *Room) handleRegister(payload []byte, from *net.UDPAddr) {
	reg, err := protocol.UnmarshalRegister(payload)
	if err != nil {
		return
	}

	t := i18n.T()

	// Version compatibility check
	if !protocol.IsCompatible(reg.Version, protocol.AppVersion) {
		r.sendKick(from, fmt.Sprintf(t.KickVersionMismatch,
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
	if !r.checkRegRate(from.IP.String()) {
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
			oldIP := addrToConnIPKey(existing.PublicAddr)
			r.ipConnMu.Lock()
			r.ipConnCount[oldIP]--
			if r.ipConnCount[oldIP] <= 0 {
				delete(r.ipConnCount, oldIP)
			}
			r.ipConnMu.Unlock()
		}
		delete(r.addrMap, fromKey)
		if r.pendingAuth > 0 {
			r.pendingAuth--
		}
		// Fall through to allow new registration
	}

	if existing := r.addrMap[fromKey]; existing != nil {
		existing.LastSeen = time.Now()
		selfIP := existing.VirtualIP
		r.mu.Unlock()
		r.sendAssignIP(selfIP, from)
		r.sendPeerInfoToClient(from)
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

	if len(r.clients) >= r.maxPlayers {
		r.ipConnMu.Lock()
		r.ipConnCount[clientIP]--
		r.ipConnMu.Unlock()
		r.mu.Unlock()
		r.sendKick(from, t.KickRoomFull)
		return
	}

	for _, c := range r.clients {
		if c.auth == authChallengeSent {
			continue
		}
		if c.authRoomID == reg.RoomID && c.Username == reg.Username {
			r.ipConnMu.Lock()
			r.ipConnCount[clientIP]--
			r.ipConnMu.Unlock()
			r.mu.Unlock()
			r.sendKick(from, t.KickDuplicateName)
			return
		}
	}

	if r.roomPass == "" {
		r.registerClientLocked(reg, from)
		return
	}

	if r.pendingAuth >= r.maxPending {
		r.ipConnMu.Lock()
		r.ipConnCount[clientIP]--
		r.ipConnMu.Unlock()
		r.mu.Unlock()
		r.sendKick(from, t.KickServerBusy)
		return
	}

	r.sendAuthChallengeLocked(reg, from)
}

func (r *Room) registerClientLocked(reg *protocol.RegisterPayload, from *net.UDPAddr) {
	t := i18n.T()
	vip := r.nextAvailableIP()
	if vip == nil {
		// Rollback the IP count increment done in handleRegister
		clientIP := addrToConnIPKey(from)
		r.ipConnMu.Lock()
		r.ipConnCount[clientIP]--
		r.ipConnMu.Unlock()
		r.mu.Unlock()
		r.sendKick(from, t.KickIPExhausted)
		return
	}

	r.markIPUsed(vip)

	c := &Client{
		Username:   reg.Username,
		VirtualIP:  vip,
		PublicAddr: from,
		LastSeen:   time.Now(),
		auth:       authNone,
		authRoomID: reg.RoomID,
	}
	r.clients[ipKey(vip)] = c
	r.addrMap[addrToRateKey(from)] = c

	// IP count already incremented in handleRegister — no duplicate increment here

	log.Printf(t.LogPlayerJoin, reg.Username, from, vip, len(r.clients))

	r.totalRegistrations.Add(1)
	if cur := uint32(len(r.clients)); cur > r.peakPlayers.Load() {
		r.peakPlayers.Store(cur)
	}

	selfIP := vip
	r.mu.Unlock()

	r.sendAssignIP(selfIP, from)
	r.sendPeerInfoToClient(from)
	r.peerInfoDirty.Store(true)
	r.markDirty()
}

func (r *Room) sendAuthChallengeLocked(reg *protocol.RegisterPayload, from *net.UDPAddr) {
	t := i18n.T()
	challenge, err := auth.GenerateChallenge()
	if err != nil {
		// Rollback the IP count increment done in handleRegister
		clientIP := addrToConnIPKey(from)
		r.ipConnMu.Lock()
		r.ipConnCount[clientIP]--
		r.ipConnMu.Unlock()
		r.mu.Unlock()
		log.Printf(t.LogChallengeFail, err)
		r.sendKick(from, t.KickInternalError)
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
	r.addrMap[addrToRateKey(from)] = c
	r.pendingAuth++
	r.mu.Unlock()

	acp := &protocol.AuthChallengePayload{Challenge: challenge, ClientAddr: from.String()}
	r.sendChecked(protocol.TypeAuthChallenge, acp.Marshal(), from)
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

	if c == nil || c.auth != authChallengeSent {
		// If c == nil, the entry was already cleaned up (e.g. by CleanupStale
		// which already rolled back ipConnCount). Don't double-decrement.
		// Only rollback if c exists but has wrong auth state (genuine anomaly).
		if c != nil {
			clientIP := addrToConnIPKey(from)
			r.ipConnMu.Lock()
			r.ipConnCount[clientIP]--
			r.ipConnMu.Unlock()
		}
		r.mu.Unlock()
		r.sendKick(from, t.KickAuthAbnormal)
		return
	}

	if time.Since(c.challengeAt) > 15*time.Second {
		delete(r.addrMap, fromKey)
		if r.pendingAuth > 0 {
			r.pendingAuth--
		}
		// Rollback the IP count increment done in handleRegister
		clientIP := addrToConnIPKey(from)
		r.ipConnMu.Lock()
		r.ipConnCount[clientIP]--
		r.ipConnMu.Unlock()
		r.mu.Unlock()
		r.sendKick(from, t.KickAuthTimeout)
		return
	}

	authKey, err := r.getAuthKey(c.authRoomID)
	if err != nil || authKey == nil {
		delete(r.addrMap, fromKey)
		if r.pendingAuth > 0 {
			r.pendingAuth--
		}
		// Rollback the IP count increment done in handleRegister
		clientIP := addrToConnIPKey(from)
		r.ipConnMu.Lock()
		r.ipConnCount[clientIP]--
		r.ipConnMu.Unlock()
		r.mu.Unlock()
		r.sendKick(from, t.KickInternalError)
		return
	}

	if !auth.VerifyHMAC(authKey, resp.HMAC, c.challenge, resp.RoomID, resp.Username, from) {
		delete(r.addrMap, fromKey)
		if r.pendingAuth > 0 {
			r.pendingAuth--
		}
		// Rollback the IP count increment done in handleRegister
		clientIP := addrToConnIPKey(from)
		r.ipConnMu.Lock()
		r.ipConnCount[clientIP]--
		r.ipConnMu.Unlock()
		r.mu.Unlock()
		log.Printf(t.LogAuthFail, resp.Username, from)
		r.authFailures.Add(1)
		r.sendKick(from, t.KickWrongPassword)
		return
	}

	log.Printf(t.LogAuthPass, resp.Username, from)
	delete(r.addrMap, fromKey)
	if r.pendingAuth > 0 {
		r.pendingAuth--
	}

	if len(r.clients) >= r.maxPlayers {
		// Rollback the IP count increment done in handleRegister
		clientIP := addrToConnIPKey(from)
		r.ipConnMu.Lock()
		r.ipConnCount[clientIP]--
		r.ipConnMu.Unlock()
		r.mu.Unlock()
		r.sendKick(from, t.KickRoomFull)
		return
	}

	for _, existing := range r.clients {
		if existing.auth == authChallengeSent {
			continue
		}
		if existing.authRoomID == resp.RoomID && existing.Username == resp.Username {
			// Rollback the IP count increment done in handleRegister
			clientIP := addrToConnIPKey(from)
			r.ipConnMu.Lock()
			r.ipConnCount[clientIP]--
			r.ipConnMu.Unlock()
			r.mu.Unlock()
			r.sendKick(from, t.KickDuplicateName)
			return
		}
	}

	reg := &protocol.RegisterPayload{RoomID: resp.RoomID, Username: resp.Username}
	r.registerClientLocked(reg, from)
}

// ── KeepAlive / Disconnect / Pong ──────────────────────────────

func (r *Room) handleKeepAlive(from *net.UDPAddr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.addrMap[addrToRateKey(from)]; c != nil {
		c.LastSeen = time.Now()
	}
}

func (r *Room) handleDisconnect(from *net.UDPAddr) {
	fromKey := addrToRateKey(from)
	r.mu.Lock()
	c := r.addrMap[fromKey]
	if c == nil {
		r.mu.Unlock()
		return
	}
	log.Printf(i18n.T().LogPlayerLeave, c.Username, c.VirtualIP)
	if c.auth == authChallengeSent {
		if r.pendingAuth > 0 {
			r.pendingAuth--
		}
		if c.PublicAddr != nil {
			ip := addrToConnIPKey(c.PublicAddr)
			r.ipConnMu.Lock()
			r.ipConnCount[ip]--
			if r.ipConnCount[ip] <= 0 {
				delete(r.ipConnCount, ip)
			}
			r.ipConnMu.Unlock()
		}
	} else {
		r.markIPFree(c.VirtualIP)
		delete(r.clients, ipKey(c.VirtualIP))
		if c.PublicAddr != nil {
			ip := addrToConnIPKey(c.PublicAddr)
			r.ipConnMu.Lock()
			r.ipConnCount[ip]--
			if r.ipConnCount[ip] <= 0 {
				delete(r.ipConnCount, ip)
			}
			r.ipConnMu.Unlock()
		}
	}
	delete(r.addrMap, fromKey)
	r.mu.Unlock()

	if r.bwLimiter != nil {
		r.bwLimiter.Remove(from)
	}
	r.peerInfoDirty.Store(true)
	r.markDirty()
}

func (r *Room) handlePong(payload []byte, from *net.UDPAddr) {
	ping, err := protocol.UnmarshalPing(payload)
	if err != nil {
		return
	}
	rtt := time.Since(time.Unix(0, ping.Timestamp))
	if rtt < 0 || rtt > 10*time.Second {
		return
	}
	r.mu.Lock()
	if c := r.addrMap[addrToRateKey(from)]; c != nil {
		c.RTT = rtt
		c.pingHistory[c.pingIdx%pingHistorySize] = rtt
		c.pingIdx++
	}
	r.mu.Unlock()
}

// ── Keepalive Cleanup ──────────────────────────────────────────

// CleanupStale removes clients that haven't been seen for 30s.
// Returns true if any clients were removed.
func (r *Room) CleanupStale() bool {
	now := time.Now()

	r.mu.RLock()
	type staleClient struct {
		key [16]byte
		c   *Client
	}
	type staleAuth struct {
		key rateKey
		c   *Client
	}
	var staleClients []staleClient
	var staleAuths []staleAuth
	for key, c := range r.clients {
		if now.Sub(c.LastSeen) > 30*time.Second {
			staleClients = append(staleClients, staleClient{key: key, c: c})
		}
	}
	for addrKey, c := range r.addrMap {
		if c.auth == authChallengeSent && now.Sub(c.challengeAt) > 30*time.Second {
			staleAuths = append(staleAuths, staleAuth{key: addrKey, c: c})
		}
	}
	r.mu.RUnlock()

	if len(staleClients) == 0 && len(staleAuths) == 0 {
		return false
	}

	r.mu.Lock()
	changed := false
	for _, sc := range staleClients {
		if cur, ok := r.clients[sc.key]; ok && cur == sc.c {
			log.Printf("%s", i18n.Format(i18n.T().ServerPeerLeave, sc.c.Username, sc.c.VirtualIP))
			r.markIPFree(sc.c.VirtualIP)
			delete(r.clients, sc.key)
			if sc.c.PublicAddr != nil {
				delete(r.addrMap, addrToRateKey(sc.c.PublicAddr))
				ip := addrToConnIPKey(sc.c.PublicAddr)
				r.ipConnMu.Lock()
				r.ipConnCount[ip]--
				if r.ipConnCount[ip] <= 0 {
					delete(r.ipConnCount, ip)
				}
				r.ipConnMu.Unlock()
			}
			changed = true
		}
	}
	for _, sa := range staleAuths {
		if cur, ok := r.addrMap[sa.key]; ok && cur == sa.c {
			delete(r.addrMap, sa.key)
			if r.pendingAuth > 0 {
				r.pendingAuth--
			}
			if sa.c.PublicAddr != nil {
				ip := addrToConnIPKey(sa.c.PublicAddr)
				r.ipConnMu.Lock()
				r.ipConnCount[ip]--
				if r.ipConnCount[ip] <= 0 {
					delete(r.ipConnCount, ip)
				}
				r.ipConnMu.Unlock()
			}
		}
	}
	r.mu.Unlock()

	if changed {
		r.markDirty()
	}
	return changed
}
