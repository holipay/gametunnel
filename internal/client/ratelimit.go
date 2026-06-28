package client

import (
	"sync"
	"time"
)

// clientSendLimiter is a simple token bucket rate limiter for client-side sends.
// It limits the total outbound data rate to prevent saturating the upload link.
type clientSendLimiter struct {
	mu       sync.Mutex
	rate     int64     // bytes per second
	burst    int64     // maximum burst size
	tokens   int64     // current tokens
	lastTime time.Time // last refill time
}

// newClientSendLimiter creates a rate limiter with the given rate (bytes/sec) and burst (bytes).
func newClientSendLimiter(rate int, burst int) *clientSendLimiter {
	return &clientSendLimiter{
		rate:     int64(rate),
		burst:    int64(burst),
		tokens:   int64(burst),
		lastTime: time.Now(),
	}
}

// allow checks if sending `size` bytes is allowed. Non-blocking.
// Returns true if allowed (tokens consumed), false if rate limited.
func (l *clientSendLimiter) allow(size int) bool {
	if l == nil {
		return true // no limiter = unlimited
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(l.lastTime).Seconds()
	l.lastTime = now

	// Guard against system clock rollback (NTP adjustments, VM migration).
	// Negative elapsed would subtract tokens and could stall all sends.
	if elapsed < 0 {
		elapsed = 0
	}

	// Refill tokens
	l.tokens += int64(elapsed * float64(l.rate))
	if l.tokens > l.burst {
		l.tokens = l.burst
	}

	// Check if we have enough tokens
	if l.tokens < int64(size) {
		return false
	}

	l.tokens -= int64(size)
	return true
}
