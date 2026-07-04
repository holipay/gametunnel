package server

import (
	"github.com/holipay/gametunnel/internal/netkey"
	"fmt"
	"net"
	"time"

	"github.com/holipay/gametunnel/internal/i18n"
)

// ── Status Info ────────────────────────────────────────────────

// RoomStatusInfo holds per-room status for the status page.
type RoomStatusInfo struct {
	RoomID      string           `json:"room_id"`
	Players     int              `json:"players"`
	MaxPlayers  int              `json:"max_players"`
	HasAuth     bool             `json:"has_auth"`
	Connections []ConnectionInfo `json:"connections,omitempty"`

	TotalRegistrations  uint64 `json:"total_registrations"`
	AuthFailures        uint64 `json:"auth_failures"`
	PeakPlayers         uint32 `json:"peak_players"`
	TotalPacketsRelay   uint64 `json:"total_packets_relay"`
	TotalPacketsDropped uint64 `json:"total_packets_dropped"`
	TotalKicks          uint64 `json:"total_kicks"`
	SendErrors          int64  `json:"send_errors"`
}

// BuildRoomStatus creates a RoomStatusInfo snapshot.
func (r *Room) BuildRoomStatus() RoomStatusInfo {
	t := i18n.T()
	now := time.Now()

	// Collect raw data under lock — no allocations from fmt.Sprintf.
	type clientSnap struct {
		username      string
		vip           string
		pubAddr       string
		idle          time.Duration
		rtt           time.Duration
		lossRate      float64
		jitter        time.Duration
		pingCount     int
		clientVersion uint16
	}
	r.mu.RLock()
	snaps := make([]clientSnap, 0, len(r.clients))
	for _, c := range r.clients {
		var pubAddr string
		if c.PublicAddr != nil {
			pubAddr = c.PublicAddr.String()
		}
		lossRate, jitter := c.PingStats()
		snaps = append(snaps, clientSnap{
			username:      c.Username,
			vip:           c.VirtualIP.String(),
			pubAddr:       pubAddr,
			idle:          now.Sub(c.GetLastSeen()),
			rtt:           c.RTT,
			lossRate:      lossRate,
			jitter:        jitter,
			pingCount:     c.pingIdx,
			clientVersion: c.clientVersion,
		})
	}
	r.mu.RUnlock()

	// Format strings outside the lock.
	conns := make([]ConnectionInfo, 0, len(snaps))
	for _, s := range snaps {
		idleStr := t.StatusJustNow
		if s.idle > time.Second {
			idleStr = fmt.Sprintf(t.StatusSecAgo, int(s.idle.Seconds()))
		}
		pingStr := "--"
		if s.rtt > 0 {
			pingStr = fmt.Sprintf("%dms", s.rtt.Milliseconds())
		}
		lossStr := "--"
		if s.pingCount > 0 {
			lossStr = fmt.Sprintf("%.0f%%", s.lossRate*100)
		}
		jitterStr := "--"
		if s.jitter > 0 {
			jitterStr = fmt.Sprintf("%dms", s.jitter.Milliseconds())
		}
		var versionStr string
		if s.clientVersion > 0 {
			major := s.clientVersion >> 8
			minor := s.clientVersion & 0xFF
			versionStr = fmt.Sprintf("v%d.%d", major, minor)
		}
		conns = append(conns, ConnectionInfo{
			Username:      s.username,
			VirtualIP:     s.vip,
			PublicAddr:    s.pubAddr,
			Idle:          idleStr,
			Ping:          pingStr,
			Loss:          lossStr,
			Jitter:        jitterStr,
			ClientVersion: versionStr,
		})
	}

	return RoomStatusInfo{
		RoomID:              r.roomID,
		Players:             len(conns),
		MaxPlayers:          r.maxPlayers,
		HasAuth:             r.roomPass != "",
		Connections:         conns,
		TotalRegistrations:  r.totalRegistrations.Load(),
		AuthFailures:        r.authFailures.Load(),
		PeakPlayers:         r.peakPlayers.Load(),
		TotalPacketsRelay:   r.totalPacketsRelay.Load(),
		TotalPacketsDropped: r.totalPacketsDropped.Load(),
		TotalKicks:          r.totalKicks.Load(),
		SendErrors:          r.sendErrors.Load(),
	}
}

// ── State Persistence ────────────────────────────────────────

// SnapshotState creates a RoomState from the current in-memory state.
func (r *Room) SnapshotState() RoomState {
	r.mu.RLock()
	defer r.mu.RUnlock()

	clients := make(map[string]ClientEntry, len(r.clients))
	for _, c := range r.clients {
		// Skip clients still in auth challenge (not fully registered)
		if c.auth == authChallengeSent {
			continue
		}
		ipStr := c.VirtualIP.String()
		clients[ipStr] = ClientEntry{
			Username:  c.Username,
			VirtualIP: ipStr,
			LastSeen:  c.GetLastSeen(),
		}
	}

	return RoomState{
		Version:   stateVersion,
		Subnet:    r.subnet.String(),
		UpdatedAt: time.Now(),
		IPBitmap:  append([]uint64(nil), r.ipBitmap...),
		Clients:   clients,
	}
}

// resolveRestoredClient handles a client that was restored from persisted state.
// When a client reconnects and its virtual IP was pre-reserved, we attach the
// real PublicAddr and return the existing IP.
// Returns the restored client if matched, nil otherwise.
// MUST be called with r.mu held.
func (r *Room) resolveRestoredClient(username string, roomID string, from *net.UDPAddr) *Client {
	clientIP := addrToConnIPKey(from)

	// Enforce maxPerIP for restored clients too.
	// Increment immediately under the same lock to prevent TOCTOU race.
	r.ipConnMu.Lock()
	ipCount := r.ipConnCount[clientIP]
	if ipCount >= r.maxPerIP {
		r.ipConnMu.Unlock()
		return nil
	}
	r.ipConnCount[clientIP]++
	r.ipConnMu.Unlock()

	// Look for a placeholder client with matching username and roomID, and no PublicAddr
	for _, c := range r.clients {
		if c.Username == username && c.authRoomID == roomID && c.PublicAddr == nil && c.auth == authNone {
			// Attach the real address
			c.PublicAddr = from
			c.SetLastSeen(time.Now())
			r.addrMap[netkey.AddrToRateKey(from)] = c

			return c
		}
	}

	// No matching client found, rollback the IP count increment
	r.decrementIPConnCount(clientIP)
	return nil
}
