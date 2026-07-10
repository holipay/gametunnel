// Package server implements the GameTunnel relay server.
package server

import (
	"sync"
	"time"
)

// tokenBucket is a mutex-protected token bucket for bandwidth limiting.
// Safe for concurrent use from multiple goroutines.
type tokenBucket struct {
	mu       sync.Mutex
	tokens   float64
	maxBurst float64
	rate     float64   // bytes per second
	lastTime time.Time // last refill time
}

// newTokenBucket creates a tokenBucket with the given rate (bytes/sec) and burst (bytes).
// The bucket starts full (burst tokens available immediately).
func newTokenBucket(rate, burst float64) *tokenBucket {
	return &tokenBucket{
		tokens:   burst,
		maxBurst: burst,
		rate:     rate,
		lastTime: time.Now(),
	}
}

// Allow checks if n bytes can be consumed right now.
// Returns true and deducts tokens if available, false otherwise.
// Non-blocking.
func (b *tokenBucket) Allow(n int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill()
	if b.tokens >= float64(n) {
		b.tokens -= float64(n)
		return true
	}
	return false
}

// LastUsed returns the time of the last refill/consume operation.
func (b *tokenBucket) LastUsed() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastTime
}

// refill adds tokens based on elapsed time. Must be called with b.mu held.
func (b *tokenBucket) refill() {
	now := time.Now()
	elapsed := now.Sub(b.lastTime).Seconds()
	b.lastTime = now
	// Guard against clock rollback (NTP adjustments, VM migration).
	if elapsed < 0 {
		elapsed = 0
	}
	b.tokens += b.rate * elapsed
	if b.tokens > b.maxBurst {
		b.tokens = b.maxBurst
	}
}
