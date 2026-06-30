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

	// Cap elapsed to 1s to prevent overflow in elapsed * rate.
	// A 1-second refill is generous — at 1 GB/s that's 1 GB worth of tokens,
	// which is far above any reasonable burst limit.
	if elapsed > 1 {
		elapsed = 1
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

// reservation represents pre-reserved rate limiter tokens.
// Created by tryReserve, then either commit() or cancel().
type reservation struct {
	limiter  *clientSendLimiter
	size     int
	reserved bool
}

// tryReserve attempts to reserve size bytes. Unlike allow(), the caller
// can cancel (refund) the reservation if the send ultimately fails.
func (l *clientSendLimiter) tryReserve(size int) *reservation {
	if l == nil {
		return &reservation{reserved: true}
	}
	tokens := l.tokens.Load()
	if tokens >= int64(size) && l.tokens.CompareAndSwap(tokens, tokens-int64(size)) {
		return &reservation{limiter: l, size: size, reserved: true}
	}
	return l.tryReserveSlow(size)
}

func (l *clientSendLimiter) tryReserveSlow(size int) *reservation {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(l.lastTime).Seconds()
	l.lastTime = now
	if elapsed < 0 {
		elapsed = 0
	}

	tokens := l.tokens.Load()
	tokens += int64(elapsed * float64(l.rate))
	if tokens > l.burst {
		tokens = l.burst
	}

	if tokens < int64(size) {
		l.tokens.Store(tokens)
		return &reservation{reserved: false}
	}

	tokens -= int64(size)
	l.tokens.Store(tokens)
	return &reservation{limiter: l, size: size, reserved: true}
}

// ok returns true if the reservation was successful (tokens available).
func (r *reservation) ok() bool { return r.reserved }

// commit is a no-op — tokens were already deducted by tryReserve.
func (r *reservation) commit() {}

// cancel refunds the reserved tokens back to the bucket.
func (r *reservation) cancel() {
	if !r.reserved || r.limiter == nil {
		return
	}
	r.limiter.mu.Lock()
	tokens := r.limiter.tokens.Load()
	tokens += int64(r.size)
	if tokens > r.limiter.burst {
		tokens = r.limiter.burst
	}
	r.limiter.tokens.Store(tokens)
	r.limiter.mu.Unlock()
}
