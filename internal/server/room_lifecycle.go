package server

import (
	"log"
	"net"
	"time"

	"github.com/holipay/gametunnel/internal/i18n"
)

// ── KeepAlive / Disconnect ─────────────────────────────────────

func (r *Room) handleKeepAlive(from *net.UDPAddr) {
	r.mu.RLock()
	c := r.addrMap[addrToRateKey(from)]
	r.mu.RUnlock()
	if c != nil {
		c.SetLastSeen(time.Now())
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
	r.lastActivity.Store(time.Now().UnixNano())
	if c.auth == authChallengeSent {
		if r.pendingAuth > 0 {
			r.pendingAuth--
		}
		if c.PublicAddr != nil {
			r.decrementIPConnCount(addrToConnIPKey(c.PublicAddr))
		}
	} else {
		r.markIPFree(c.VirtualIP)
		delete(r.clients, ipKey(c.VirtualIP))
		if c.PublicAddr != nil {
			r.decrementIPConnCount(addrToConnIPKey(c.PublicAddr))
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
		key    [16]byte
		aKey   rateKey
		connKey connIPKey
		c      *Client
	}
	type staleAuth struct {
		key     rateKey
		connKey connIPKey
		c       *Client
	}
	var staleClients []staleClient
	var staleAuths []staleAuth
	for key, c := range r.clients {
		if now.Sub(c.GetLastSeen()) > 30*time.Second {
			sc := staleClient{key: key, c: c}
			if c.PublicAddr != nil {
				sc.aKey = addrToRateKey(c.PublicAddr)
				sc.connKey = addrToConnIPKey(c.PublicAddr)
			}
			staleClients = append(staleClients, sc)
		}
	}
	for addrKey, c := range r.addrMap {
		if c.auth == authChallengeSent && now.Sub(c.challengeAt) > 30*time.Second {
			sa := staleAuth{key: addrKey, c: c}
			if c.PublicAddr != nil {
				sa.connKey = addrToConnIPKey(c.PublicAddr)
			}
			staleAuths = append(staleAuths, sa)
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
			if time.Since(cur.GetLastSeen()) <= 30*time.Second {
				continue // no longer stale — received keepalive while we held RUnlock
			}
			log.Printf("%s", i18n.Format(i18n.T().ServerPeerLeave, sc.c.Username, sc.c.VirtualIP))
			r.markIPFree(sc.c.VirtualIP)
			delete(r.clients, sc.key)
			if sc.c.PublicAddr != nil {
				delete(r.addrMap, sc.aKey)
				r.decrementIPConnCount(sc.connKey)
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
				r.decrementIPConnCount(sa.connKey)
			}
		}
	}
	r.mu.Unlock()

	if changed {
		r.markDirty()
	}
	return changed
}
