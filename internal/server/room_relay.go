package server

import (
	"github.com/holipay/gametunnel/internal/netkey"
	"log"
	"net"
	"time"

	"github.com/holipay/gametunnel/internal/netutil"
	"github.com/holipay/gametunnel/internal/protocol"
)

// tcpBridgeAddr is the synthetic UDP address assigned to TCP bridge clients.
// Bandwidth limiting is skipped for this address (TCP's own flow control
// provides backpressure).
var tcpBridgeAddr = net.IPv4(127, 0, 0, 254)

// appendAddr appends "ip:port" to buf, delegating to protocol.AppendAddrStr.
func appendAddr(buf []byte, addr *net.UDPAddr) []byte {
	return protocol.AppendAddrStr(buf, addr)
}

// relayLog prints a debug log message when verbose mode is enabled.
func (r *Room) relayLog(format string, args ...any) {
	if r.verbose {
		log.Printf("[relay] "+format, args...)
	}
}

// tryMigrateAddrMap attempts to find and migrate a client when the addrMap
// lookup fails due to NAT rebinding or source port changes.
// The client is located by its VIP (srcIP from the relay payload), and the
// session token is validated before updating the mapping.
// Must NOT be called with r.mu held. Returns the client on success, nil otherwise.
func (r *Room) tryMigrateAddrMap(from *net.UDPAddr, srcIP net.IP, payload []byte) *Client {
	vipKey := netkey.IPKey(srcIP)

	r.mu.RLock()
	c, ok := r.clients[vipKey]
	if !ok {
		r.relayLog("[relay] migrate: no client found for vip=%s", srcIP)
		r.mu.RUnlock()
		return nil
	}

	// Validate session token before migrating to prevent hijacking.
	if c.clientVersion >= protocol.MinTokenVersion && c.HasSessionToken() {
		if len(payload) <= 8 {
			r.mu.RUnlock()
			return nil
		}
		flags, tokenOff, _ := protocol.ParseDataHeader(payload)
		if flags&protocol.DataFlagHasToken != 0 {
			if len(payload) < tokenOff+protocol.DataTokenLen {
				r.mu.RUnlock()
				return nil
			}
			token := payload[tokenOff : tokenOff+protocol.DataTokenLen]
			if !c.ValidateSessionToken(token) {
				r.relayLog("[relay] migrate: token validation FAILED for %s", c.Username)
				r.mu.RUnlock()
				return nil
			}
			r.relayLog("[relay] migrate: token validation OK for %s", c.Username)
		}
	}
	r.mu.RUnlock()

	// Upgrade to write lock for addrMap update.
	r.mu.Lock()
	// Re-check — another goroutine may have handled the migration already.
	if existing := r.addrMap[netkey.AddrToRateKey(from)]; existing != nil {
		r.mu.Unlock()
		return existing
	}
	// Re-check that the client still exists.
	if _, stillThere := r.clients[vipKey]; !stillThere {
		r.mu.Unlock()
		return nil
	}

	// Remove old addrMap entry.
	if c.PublicAddr != nil {
		delete(r.addrMap, netkey.AddrToRateKey(c.PublicAddr))
	}
	// Update client address.
	c.PublicAddr = from
	c.SetLastSeen(time.Now())
	r.addrMap[netkey.AddrToRateKey(from)] = c
	r.relayLog("[relay] addrMap migration: %s (%s) → %s", c.Username, c.VirtualIP, from)
	r.mu.Unlock()
	return c
}

// ── Relay ──────────────────────────────────────────────────────

func (r *Room) handleRelay(payload []byte, from *net.UDPAddr) {
	if len(payload) < 8 {
		return
	}

	var srcIP, dstIP [4]byte
	copy(srcIP[:], payload[0:4])
	copy(dstIP[:], payload[4:8])

	// Determine broadcast before taking the lock — r.subnet is immutable
	// after Room creation, so this is safe outside the critical section.
	isBroadcast := netutil.IsRelayTargetRaw(dstIP, r.subnet)

	r.relayLog("[relay] received relay from %s: src=%d.%d.%d.%d dst=%d.%d.%d.%d len=%d",
		from, srcIP[0], srcIP[1], srcIP[2], srcIP[3],
		dstIP[0], dstIP[1], dstIP[2], dstIP[3], len(payload))

	r.mu.RLock()
	sender := r.addrMap[netkey.AddrToRateKey(from)]
	if sender == nil {
		// addrMap miss — NAT rebinding or source port change.
		// Try to find client by VIP and validate session token.
		r.mu.RUnlock()
		sender = r.tryMigrateAddrMap(from, srcIP[:], payload)
		if sender == nil {
			r.relayLog("[relay] sender not found in addrMap")
			return
		}
		r.mu.RLock()
	}

	r.relayLog("[relay] sender=%s vip=%s", sender.Username, sender.VirtualIP)

	// Compare raw 4-byte VIP instead of net.IP.Equal to avoid heap allocation
	senderVIP := sender.VirtualIP.To4()
	if senderVIP == nil || [4]byte(senderVIP) != srcIP {
		r.relayLog("[relay] srcIP mismatch")
		r.mu.RUnlock()
		return
	}

	// Session token validation (v1.7+): if client sent a token, verify it.
	if sender.clientVersion >= protocol.MinTokenVersion && sender.HasSessionToken() {
		if len(payload) <= 8 {
			r.relayLog("[relay] payload too short for token check")
			r.mu.RUnlock()
			return
		}
		flags, tokenOff, isNew := protocol.ParseDataHeader(payload)
		r.relayLog("[relay] token check: flags=0x%x tokenOff=%d isNew=%v", flags, tokenOff, isNew)
		if flags&protocol.DataFlagHasToken != 0 {
			if len(payload) < tokenOff+protocol.DataTokenLen {
				r.relayLog("[relay] payload too short for token")
				r.mu.RUnlock()
				return
			}
			token := payload[tokenOff : tokenOff+protocol.DataTokenLen]
			if !sender.ValidateSessionToken(token) {
				r.relayLog("[relay] token validation FAILED")
				r.mu.RUnlock()
				return
			}
			r.relayLog("[relay] token validation OK")
		}
	}

	r.relayLog("[relay] isBroadcast=%v", isBroadcast)

	var stackTargets [maxInlineTargets]*net.UDPAddr
	targets := stackTargets[:0]

	if isBroadcast {
		for _, c := range r.clients {
			if c != sender && c.PublicAddr != nil {
				targets = append(targets, c.PublicAddr)
			}
		}
	} else {
		if dst, ok := r.clients[netkey.IPKey4(dstIP)]; ok && dst.PublicAddr != nil {
			targets = append(targets, dst.PublicAddr)
		}
	}
	r.mu.RUnlock()

	if len(targets) == 0 {
		r.relayLog("[relay] no targets to relay to")
		return
	}

	r.relayLog("[relay] forwarding to %d targets", len(targets))
	// Encode once, but DO NOT pool the buffer — sendCheckedRaw enqueues
	// the slice into the async send queue, and the consumer reads it later.
	// Returning the buffer to the pool before the consumer reads causes
	// data corruption (use-after-free on the pooled buffer).
	// Encrypted rooms skip outer CRC32: AEAD already provides integrity.
	// Unencrypted rooms keep CRC32 for DecodeChecked on the client.
	var encoded []byte
	if r.roomPass != "" {
		encoded = protocol.Encode(protocol.TypeData, payload)
	} else {
		encoded = protocol.EncodeChecked(protocol.TypeData, payload)
	}
	packetSize := len(encoded)
	if isBroadcast {
		// Broadcast packets bypass per-client bandwidth limits.
		// Broadcasts are game-critical (LAN discovery, game state sync)
		// and already bounded by the sender's own rate — per-recipient
		// limiting just adds latency for no benefit.
		for _, addr := range targets {
			r.sendCheckedRawBypass(encoded, addr)
		}
	} else {
		for _, addr := range targets {
			// Skip bandwidth limiting for TCP bridge clients (synthetic tcpBridgeAddr)
			// since their bandwidth is bounded by the TCP connection itself.
			if r.bwLimiter == nil || addr.IP.Equal(tcpBridgeAddr) || r.bwLimiter.Allow(addr, packetSize) {
				r.sendCheckedRaw(encoded, addr)
			}
		}
	}
	r.totalPacketsRelay.Add(1)
}

func (r *Room) handleHolePunch(payload []byte, from *net.UDPAddr) {
	if len(payload) < 4 {
		return
	}
	var dstIP [4]byte
	copy(dstIP[:], payload[:4])

	r.mu.RLock()
	src, ok1 := r.addrMap[netkey.AddrToRateKey(from)]
	dst, ok2 := r.clients[netkey.IPKey4(dstIP)]
	var dstAddr *net.UDPAddr
	if ok2 {
		dstAddr = dst.PublicAddr
	}
	r.mu.RUnlock()

	if !ok1 || !ok2 || dstAddr == nil {
		return
	}

	if src.VirtualIP == nil {
		return
	}

	// Build punch data: 4 bytes virtual IP + "ip:port" address
	// Pre-allocate typical size to avoid re-allocation
	punchData := make([]byte, 0, 4+21) // 4 + typical "1.2.3.4:12345"
	punchData = append(punchData, src.VirtualIP.To4()...)
	punchData = appendAddr(punchData, from)
	r.sendChecked(protocol.TypeHolePunch, punchData, dstAddr)
}

// ── Peer Info ──────────────────────────────────────────────────

func (r *Room) handlePeerRequest(from *net.UDPAddr) {
	r.mu.RLock()
	c := r.addrMap[netkey.AddrToRateKey(from)]
	r.mu.RUnlock()
	if c == nil {
		return
	}
	r.sendPeerInfoToClient(from)
}

func (r *Room) sendPeerInfoToClient(target *net.UDPAddr) {
	encoded := r.getEncodedPeerInfo()
	r.sendCheckedRaw(encoded, target)
}

func (r *Room) sendPeerInfoBroadcast() {
	r.mu.RLock()
	if len(r.clients) == 0 {
		r.mu.RUnlock()
		return
	}

	// Use stack-allocated array for small rooms to avoid heap allocation
	var stackTargets [maxInlineTargets]*net.UDPAddr
	targets := stackTargets[:0]
	for _, c := range r.clients {
		if c.PublicAddr != nil {
			targets = append(targets, c.PublicAddr)
		}
	}
	r.mu.RUnlock()

	encoded := r.getEncodedPeerInfo()
	for _, addr := range targets {
		r.sendCheckedRawBypass(encoded, addr)
	}
}

func (r *Room) getEncodedPeerInfo() []byte {
	now := time.Now()
	r.peerInfoMu.Lock()
	if r.peerInfoEncoded != nil && now.Sub(r.peerInfoCachedAt) < peerInfoCacheTTL {
		encoded := r.peerInfoEncoded
		r.peerInfoMu.Unlock()
		return encoded
	}

	r.mu.RLock()
	peers := protocol.GetPeerInfoPayload()
	peers.Peers = peers.Peers[:0] // reset slice but keep capacity
	for _, c := range r.clients {
		peers.Peers = append(peers.Peers, protocol.PeerInfoEntry{
			VirtualIP:  c.VirtualIP,
			PublicAddr: c.PublicAddr,
			Username:   c.Username,
			NATType:    protocol.NATType(c.NATType.Load()),
		})
	}
	r.mu.RUnlock()

	encoded := protocol.EncodeChecked(protocol.TypePeerInfo, peers.Marshal())
	protocol.PutPeerInfoPayload(peers)
	r.peerInfoEncoded = encoded
	r.peerInfoCachedAt = now
	r.peerInfoMu.Unlock()
	return encoded
}

// invalidatePeerInfoCache marks peer info as dirty AND clears the encoded
// cache, so the next getEncodedPeerInfo() call rebuilds from current state.
// This prevents serving stale peer lists within the 50ms cache TTL window
// after a client disconnects or a new client joins.
func (r *Room) invalidatePeerInfoCache() {
	r.peerInfoDirty.Store(true)
	r.peerInfoMu.Lock()
	r.peerInfoEncoded = nil
	r.peerInfoMu.Unlock()
}
