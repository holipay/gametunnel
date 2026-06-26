package server

import (
	"log"
	"net"
	"time"

	"github.com/holipay/gametunnel/internal/i18n"
)

// ── KeepAlive / Disconnect ─────────────────────────────────────

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
	r.invalidatePeerInfoCache()
	r.markDirty()
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
