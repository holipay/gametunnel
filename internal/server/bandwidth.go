package server

import (
	"net"
	"sync"
	"time"

	"github.com/holipay/gametunnel/internal/netutil"
)

const (
	// defaultBandwidthLimit is the default per-client outbound bandwidth
	// limit in bytes per second (50 Mbps).
	defaultBandwidthLimit = 50 * 1024 * 1024 / 8 // 6.25 MB/s = 50 Mbps

	// bandwidthBurst is the max burst size in bytes. Allows short spikes
	// without letting a client monopolize the link.
	// 512 KB ≈ ~340 full-size UDP frames, enough for burst game snapshots.
	bandwidthBurst = 512 * 1024
)

// BandwidthLimiter enforces per-client outbound bandwidth limits.
// Each destination client gets its own token bucket.
type BandwidthLimiter struct {
	limit   int
	burst   int
	buckets sync.Map // rateKey → *tokenBucket
}

// NewBandwidthLimiter creates a new limiter.
// limitBytesPerSec is the per-client outbound bandwidth limit in bytes/sec.
// If <= 0, defaultBandwidthLimit is used.
func NewBandwidthLimiter(limitBytesPerSec int) *BandwidthLimiter {
	if limitBytesPerSec <= 0 {
		limitBytesPerSec = defaultBandwidthLimit
	}
	return &BandwidthLimiter{
		limit: limitBytesPerSec,
		burst: bandwidthBurst,
	}
}

// Enabled returns true if bandwidth limiting is active.
func (bl *BandwidthLimiter) Enabled() bool {
	return bl != nil && bl.limit > 0
}

// Allow checks if a packet of `size` bytes can be sent to `dest` right now.
// Returns true if allowed (tokens consumed), false if should be dropped.
// Non-blocking.
func (bl *BandwidthLimiter) Allow(dest *net.UDPAddr, size int) bool {
	if !bl.Enabled() {
		return true
	}
	b := bl.getBucket(dest)
	return b.Allow(size)
}

// getBucket returns (or creates) the token bucket for a destination.
func (bl *BandwidthLimiter) getBucket(dest *net.UDPAddr) *tokenBucket {
	key := netutil.AddrToRateKey(dest)
	if v, ok := bl.buckets.Load(key); ok {
		return v.(*tokenBucket)
	}
	b := newTokenBucket(float64(bl.limit), float64(bl.burst))
	actual, _ := bl.buckets.LoadOrStore(key, b)
	return actual.(*tokenBucket)
}

// Remove deletes the bucket for a client (call on disconnect to free memory).
func (bl *BandwidthLimiter) Remove(dest *net.UDPAddr) {
	if !bl.Enabled() {
		return
	}
	bl.buckets.Delete(netutil.AddrToRateKey(dest))
}

// Cleanup removes stale buckets that haven't been used in the given duration.
func (bl *BandwidthLimiter) Cleanup(stale time.Duration) {
	if !bl.Enabled() {
		return
	}
	cutoff := time.Now().Add(-stale)
	bl.buckets.Range(func(key, value any) bool {
		b := value.(*tokenBucket)
		if b.LastUsed().Before(cutoff) {
			bl.buckets.Delete(key)
		}
		return true
	})
}

// Count returns the number of active buckets.
func (bl *BandwidthLimiter) Count() int {
	if !bl.Enabled() {
		return 0
	}
	n := 0
	bl.buckets.Range(func(_, _ any) bool { n++; return true })
	return n
}
