// Package ratelimit provides a token bucket rate limiter shared by
// client and server. Tokens represent bytes; the bucket refills at
// a constant rate up to a burst cap.
package ratelimit

import (
	"sync"
	"time"
)

// TokenBucket is a mutex-protected token bucket for bandwidth limiting.
// Safe for concurrent use from multiple goroutines.
type TokenBucket struct {
	mu       sync.Mutex
	tokens   float64
	maxBurst float64
	rate     float64   // bytes per second
	lastTime time.Time // last refill time
}

// New creates a TokenBucket with the given rate (bytes/sec) and burst (bytes).
// The bucket starts full (burst tokens available immediately).
func New(rate, burst float64) *TokenBucket {
	return &TokenBucket{
		tokens:   burst,
		maxBurst: burst,
		rate:     rate,
		lastTime: time.Now(),
	}
}

// Allow checks if n bytes can be consumed right now.
// Returns true and deducts tokens if available, false otherwise.
// Non-blocking.
func (b *TokenBucket) Allow(n int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill()
	if b.tokens >= float64(n) {
		b.tokens -= float64(n)
		return true
	}
	return false
}

// Tokens returns the current number of available tokens (approximate).
func (b *TokenBucket) Tokens() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill()
	return b.tokens
}

// LastUsed returns the time of the last refill/consume operation.
// Used by BucketMap cleanup to detect stale buckets.
func (b *TokenBucket) LastUsed() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastTime
}

// refill adds tokens based on elapsed time. Must be called with b.mu held.
func (b *TokenBucket) refill() {
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

// BucketMap maps arbitrary keys to TokenBuckets. Used for per-client
// or per-destination rate limiting.
type BucketMap struct {
	mu      sync.Mutex
	buckets map[string]*TokenBucket
	rate    float64
	burst   float64
}

// NewBucketMap creates a BucketMap where each new bucket gets the given
// rate and burst parameters.
func NewBucketMap(rate, burst float64) *BucketMap {
	return &BucketMap{
		buckets: make(map[string]*TokenBucket),
		rate:    rate,
		burst:   burst,
	}
}

// Get returns the bucket for the given key, creating one if it doesn't exist.
func (m *BucketMap) Get(key string) *TokenBucket {
	m.mu.Lock()
	b, ok := m.buckets[key]
	if !ok {
		b = New(m.rate, m.burst)
		m.buckets[key] = b
	}
	m.mu.Unlock()
	return b
}

// Remove deletes the bucket for the given key.
func (m *BucketMap) Remove(key string) {
	m.mu.Lock()
	delete(m.buckets, key)
	m.mu.Unlock()
}

// Cleanup removes buckets that haven't been used since cutoff.
func (m *BucketMap) Cleanup(cutoff time.Time) {
	m.mu.Lock()
	for k, b := range m.buckets {
		b.mu.Lock()
		stale := b.lastTime.Before(cutoff)
		b.mu.Unlock()
		if stale {
			delete(m.buckets, k)
		}
	}
	m.mu.Unlock()
}

// Count returns the number of active buckets.
func (m *BucketMap) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.buckets)
}
