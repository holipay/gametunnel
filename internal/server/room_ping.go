package server

import (
	"github.com/holipay/gametunnel/internal/netkey"
	"context"
	"net"
	"time"

	"github.com/holipay/gametunnel/internal/protocol"
)

// ── Peer Info Broadcasting ─────────────────────────────────────

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

// ── Ping/Pong ──────────────────────────────────────────────────

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

		// Snapshot client list under read lock, then release before encoding
		// and sending to minimize time spent holding the write lock.
		r.mu.RLock()
		if len(r.clients) == 0 {
			r.mu.RUnlock()
			continue
		}
		type pingTarget struct {
			c     *Client
			addr  *net.UDPAddr
		}
		targets := make([]pingTarget, 0, len(r.clients))
		for _, c := range r.clients {
			if c.PublicAddr == nil {
				continue
			}
			targets = append(targets, pingTarget{c: c, addr: c.PublicAddr})
		}
		r.mu.RUnlock()

		ts := now.UnixNano()
		ping := &protocol.PingPayload{Timestamp: ts}
		encoded := protocol.EncodeChecked(protocol.TypePing, ping.Marshal())

		// Write lock: mark missed pings, update fields, send pings.
		r.mu.Lock()
		// Mark previous pings as missed if no pong received within 2*interval.
		for _, c := range r.clients {
			if !c.lastPingSent.IsZero() && now.Sub(c.lastPingSent) > 2*pingInterval {
				c.pingHistory[c.pingIdx%pingHistorySize] = 0 // missed
				c.pingIdx++
			}
		}
		for _, t := range targets {
			// Client may have disconnected between snapshot and now.
			if t.c.PublicAddr == nil {
				continue
			}
			t.c.pingSeq++
			t.c.lastPingSent = now
			t.c.lastPingSeq = t.c.pingSeq
			r.sendCheckedRaw(encoded, t.addr)
		}
		r.mu.Unlock()
	}
}

// handlePong processes a pong response from a client and records RTT.
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
	if c := r.addrMap[netkey.AddrToRateKey(from)]; c != nil {
		c.RTT = rtt
		c.pingHistory[c.pingIdx%pingHistorySize] = rtt
		c.pingIdx++
	}
	r.mu.Unlock()
}
