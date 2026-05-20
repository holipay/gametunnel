package server

import (
	"context"
	"net"
	"sync"
	"time"
)

const (
	// defaultBandwidthLimit is the default per-client outbound bandwidth
	// limit in bytes per second (10 Mbps).
	defaultBandwidthLimit = 10 * 1024 * 1024 / 8 // 1.25 MB/s = 10 Mbps

	// bandwidthBurst is the max burst size in bytes. Allows short spikes
	// without letting a client monopolize the link.
	// 128 KB ≈ ~85 full-size UDP frames, enough for a game snapshot.
	bandwidthBurst = 128 * 1024

	// bandwidthTimeout is how long WaitN blocks before giving up.
	// If a client can't get bandwidth within this window, the packet is dropped.
	bandwidthTimeout = 50 * time.Millisecond
)

// ── Token Bucket (lock-free per-client) ─────────────────────────

// clientBucket is a per-client token bucket for outbound bandwidth limiting.
// Tokens represent bytes. Refilled at a constant rate up to a burst cap.
type clientBucket struct {
	mu       sync.Mutex
	tokens   float64   // available bytes
	maxBurst float64   // burst cap in bytes
	rate     float64   // bytes per second
	lastTime time.Time // last refill time
}

func newClientBucket(rate float64, burst float64) *clientBucket {
	return &clientBucket{
		tokens:   burst, // start full
		maxBurst: burst,
		rate:     rate,
		lastTime: time.Now(),
	}
}

// tryTake attempts to take n bytes worth of tokens. Returns true if allowed.
// Non-blocking: returns false immediately if not enough tokens.
func (b *clientBucket) tryTake(n int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.refill()
	if b.tokens >= float64(n) {
		b.tokens -= float64(n)
		return true
	}
	return false
}

// waitTake blocks until n bytes worth of tokens are available, or times out.
func (b *clientBucket) waitTake(ctx context.Context, n int) bool {
	deadline := time.Now().Add(bandwidthTimeout)

	for {
		b.mu.Lock()
		b.refill()
		if b.tokens >= float64(n) {
			b.tokens -= float64(n)
			b.mu.Unlock()
			return true
		}
		// Calculate how long until enough tokens are available
		need := float64(n) - b.tokens
		wait := time.Duration(need/b.rate*float64(time.Second)) + time.Millisecond
		b.mu.Unlock()

		if time.Now().After(deadline) {
			return false
		}
		if wait > 10*time.Millisecond {
			wait = 10 * time.Millisecond
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

// refill adds tokens based on elapsed time. Must be called with b.mu held.
func (b *clientBucket) refill() {
	now := time.Now()
	elapsed := now.Sub(b.lastTime).Seconds()
	b.lastTime = now
	b.tokens += b.rate * elapsed
	if b.tokens > b.maxBurst {
		b.tokens = b.maxBurst
	}
}

// ── Bandwidth Limiter ───────────────────────────────────────────

// BandwidthLimiter enforces per-client outbound bandwidth limits.
// Each destination client gets its own token bucket.
type BandwidthLimiter struct {
	limit    int // bytes per second per client
	burst    int // max burst in bytes
	buckets  sync.Map // map[rateKey]*clientBucket
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
	return b.tryTake(size)
}

// Wait blocks until a packet of `size` bytes can be sent to `dest`.
// Returns false if timed out or context cancelled.
func (bl *BandwidthLimiter) Wait(ctx context.Context, dest *net.UDPAddr, size int) bool {
	if !bl.Enabled() {
		return true
	}
	b := bl.getBucket(dest)
	return b.waitTake(ctx, size)
}

// getBucket returns (or creates) the token bucket for a destination.
func (bl *BandwidthLimiter) getBucket(dest *net.UDPAddr) *clientBucket {
	key := addrToRateKey(dest)
	if v, ok := bl.buckets.Load(key); ok {
		return v.(*clientBucket)
	}
	b := newClientBucket(float64(bl.limit), float64(bl.burst))
	actual, _ := bl.buckets.LoadOrStore(key, b)
	return actual.(*clientBucket)
}

// Remove deletes the bucket for a client (call on disconnect to free memory).
func (bl *BandwidthLimiter) Remove(dest *net.UDPAddr) {
	if !bl.Enabled() {
		return
	}
	bl.buckets.Delete(addrToRateKey(dest))
}

// Cleanup removes stale buckets that haven't been used in the given duration.
// Call periodically to prevent memory leaks from disconnected clients.
func (bl *BandwidthLimiter) Cleanup(stale time.Duration) {
	if !bl.Enabled() {
		return
	}
	cutoff := time.Now().Add(-stale)
	bl.buckets.Range(func(key, value interface{}) bool {
		b := value.(*clientBucket)
		b.mu.Lock()
		stale := b.lastTime.Before(cutoff)
		b.mu.Unlock()
		if stale {
			bl.buckets.Delete(key)
		}
		return true
	})
}
