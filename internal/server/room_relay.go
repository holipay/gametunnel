package server

import (
	"net"
	"sync"
	"time"

	"github.com/holipay/gametunnel/internal/protocol"
)

// ── Relay ──────────────────────────────────────────────────────

// relayBufPool reuses buffers for relay packet encoding to reduce GC pressure
// on the hot path (every relayed game data packet).
var relayBufPool = sync.Pool{
	New: func() interface{} { return make([]byte, 0, protocol.MaxPacketLen) },
}

func (r *Room) handleRelay(payload []byte, from *net.UDPAddr) {
	if len(payload) < 8 {
		return
	}

	srcIP := net.IP(payload[0:4])
	dstIP := net.IP(payload[4:8])

	r.mu.RLock()
	sender := r.addrMap[addrToRateKey(from)]
	if sender == nil {
		r.mu.RUnlock()
		return
	}

	if !srcIP.Equal(sender.VirtualIP) {
		r.mu.RUnlock()
		return
	}

	isBroadcast := protocol.IsRelayTarget(dstIP, r.subnet)

	var stackTargets [maxInlineTargets]*net.UDPAddr
	targets := stackTargets[:0]

	if isBroadcast {
		for _, c := range r.clients {
			if c != sender {
				targets = append(targets, c.PublicAddr)
			}
		}
	} else {
		if dst, ok := r.clients[ipKey(dstIP)]; ok {
			targets = append(targets, dst.PublicAddr)
		}
	}
	r.mu.RUnlock()

	if len(targets) == 0 {
		return
	}
	buf := relayBufPool.Get().([]byte)[:0]
	encoded := protocol.AppendEncodeChecked(buf, protocol.TypeData, payload)
	packetSize := len(encoded)
	for _, addr := range targets {
		if r.bwLimiter == nil || r.bwLimiter.Allow(addr, packetSize) {
			r.sendCheckedRaw(encoded, addr)
		}
	}
	relayBufPool.Put(encoded[:cap(encoded)])
	r.totalPacketsRelay.Add(1)
}

func (r *Room) handleHolePunch(payload []byte, from *net.UDPAddr) {
	if len(payload) < 4 {
		return
	}
	dstIP := net.IP(payload[:4])

	r.mu.RLock()
	src, ok1 := r.addrMap[addrToRateKey(from)]
	dst, ok2 := r.clients[ipKey(dstIP)]
	r.mu.RUnlock()

	if !ok1 || !ok2 || dst.PublicAddr == nil {
		return
	}

	if src.VirtualIP == nil {
		return
	}

	addrStr := from.String()
	punchData := make([]byte, 4+len(addrStr))
	copy(punchData[:4], src.VirtualIP.To4())
	copy(punchData[4:], []byte(addrStr))
	r.sendChecked(protocol.TypeHolePunch, punchData, dst.PublicAddr)
}

// ── Peer Info ──────────────────────────────────────────────────

func (r *Room) handlePeerRequest(from *net.UDPAddr) {
	r.mu.RLock()
	c := r.addrMap[addrToRateKey(from)]
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
		targets = append(targets, c.PublicAddr)
	}
	r.mu.RUnlock()

	encoded := r.getEncodedPeerInfo()
	for _, addr := range targets {
		r.sendCheckedRaw(encoded, addr)
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
