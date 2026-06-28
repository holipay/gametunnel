package server

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/protocol"
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
		username  string
		vip       string
		pubAddr   string
		idle      time.Duration
		rtt       time.Duration
		lossRate  float64
		jitter    time.Duration
		pingCount int
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
			username:  c.Username,
			vip:       c.VirtualIP.String(),
			pubAddr:   pubAddr,
			idle:      now.Sub(c.GetLastSeen()),
			rtt:       c.RTT,
			lossRate:  lossRate,
			jitter:    jitter,
			pingCount: c.pingIdx,
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
		conns = append(conns, ConnectionInfo{
			Username:   s.username,
			VirtualIP:  s.vip,
			PublicAddr: s.pubAddr,
			Idle:       idleStr,
			Ping:       pingStr,
			Loss:       lossStr,
			Jitter:     jitterStr,
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

// ── Room Lifecycle Loops ─────────────────────────────────────

// peerInfoLoop periodically checks the dirty flag and broadcasts PeerInfo.
// This coalesces rapid join/leave events into a single broadcast per interval.
func (r *Room) peerInfoLoop(ctx context.Context) {
	ticker := time.NewTicker(peerInfoInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-r.done:
			return
		case <-ticker.C:
		}

		if r.peerInfoDirty.CompareAndSwap(true, false) {
			r.sendPeerInfoBroadcast()
		}
	}
}

// pingLoop periodically sends TypePing to all authenticated clients
// and tracks timeout (missed pong) for loss rate calculation.
func (r *Room) pingLoop(ctx context.Context) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-r.done:
			return
		case <-ticker.C:
		}

		now := time.Now()

		r.mu.Lock()
		if len(r.clients) == 0 {
			r.mu.Unlock()
			continue
		}

		// Mark previous pings as missed if no pong received within 2*interval.
		for _, c := range r.clients {
			if !c.lastPingSent.IsZero() && now.Sub(c.lastPingSent) > 2*pingInterval {
				c.pingHistory[c.pingIdx%pingHistorySize] = 0 // missed
				c.pingIdx++
			}
		}

		// Send pings and record sequence/time.
		ts := now.UnixNano()
		ping := &protocol.PingPayload{Timestamp: ts}
		encoded := protocol.EncodeChecked(protocol.TypePing, ping.Marshal())
		for _, c := range r.clients {
			if c.PublicAddr == nil {
				continue // restored from persistence, not yet reconnected
			}
			c.pingSeq++
			c.lastPingSent = now
			c.lastPingSeq = c.pingSeq
			r.sendCheckedRaw(encoded, c.PublicAddr)
		}
		r.mu.Unlock()
	}
}

// ── Pong Handling ──────────────────────────────────────────────

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
	// Look for a placeholder client with matching username and roomID, and no PublicAddr
	for _, c := range r.clients {
		if c.Username == username && c.authRoomID == roomID && c.PublicAddr == nil && c.auth == authNone {
			// Attach the real address
			c.PublicAddr = from
			c.SetLastSeen(time.Now())
			r.addrMap[addrToRateKey(from)] = c

			// Track per-IP connection count
			clientIP := addrToConnIPKey(from)
			r.ipConnMu.Lock()
			r.ipConnCount[clientIP]++
			r.ipConnMu.Unlock()

			return c
		}
	}
	return nil
}
