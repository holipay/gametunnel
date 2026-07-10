package server

import (
	"github.com/holipay/gametunnel/internal/netutil"
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
	if c := r.addrMap[netutil.AddrToRateKey(from)]; c != nil {
		c.RTT = rtt
		c.pingHistory[c.pingIdx%pingHistorySize] = rtt
		c.pingIdx++
	}
	r.mu.Unlock()
}
