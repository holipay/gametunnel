package client

import (
	"sync"
	"sync/atomic"
	"time"
)

// clientSendLimiter is a token bucket rate limiter for client-side sends.
// Uses an atomic fast path for the common case (tokens available) and
// falls back to a mutex only for refill logic when tokens are low.
type clientSendLimiter struct {
	mu       sync.Mutex
	rate     int64     // bytes per second
	burst    int64     // maximum burst size
	tokens   atomic.Int64 // current tokens (lock-free fast path)
	lastTime time.Time // last refill time (protected by mu)
}

// newClientSendLimiter creates a rate limiter with the given rate (bytes/sec) and burst (bytes).
func newClientSendLimiter(rate int, burst int) *clientSendLimiter {
	l := &clientSendLimiter{
		rate:     int64(rate),
		burst:    int64(burst),
		lastTime: time.Now(),
	}
	l.tokens.Store(int64(burst))
	return l
}

// allow checks if sending `size` bytes is allowed. Non-blocking.
// Returns true if allowed (tokens consumed), false if rate limited.
func (l *clientSendLimiter) allow(size int) bool {
	if l == nil {
		return true // no limiter = unlimited
	}

	// Fast path: atomically try to deduct tokens without lock.
	// Covers >90% of calls under normal load.
	tokens := l.tokens.Load()
	if tokens >= int64(size) && l.tokens.CompareAndSwap(tokens, tokens-int64(size)) {
		return true
	}

	// Slow path: tokens low or CAS contention — lock and refill.
	return l.allowSlow(size)
}

// allowSlow handles the refill-and-check logic under mutex.
func (l *clientSendLimiter) allowSlow(size int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(l.lastTime).Seconds()
	l.lastTime = now

	// Guard against system clock rollback (NTP adjustments, VM migration).
	if elapsed < 0 {
		elapsed = 0
	}

	// Refill tokens
	tokens := l.tokens.Load()
	tokens += int64(elapsed * float64(l.rate))
	if tokens > l.burst {
		tokens = l.burst
	}

	// Check if we have enough tokens
	if tokens < int64(size) {
		l.tokens.Store(tokens)
		return false
	}

	tokens -= int64(size)
	l.tokens.Store(tokens)
	return true
}
