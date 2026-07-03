package server

import (
	"github.com/holipay/gametunnel/internal/netkey"
	"context"
	"net"
	"sync"
	"time"
)

// ── Packet Rate Limiting ───────────────────────────────────────

const (
	rateLimit    = 500 // max packets per window per client
	rateInterval = time.Second

	// rateShardCount is the number of independent rate-limiting shards.
	// Each shard has its own mutex, reducing lock contention by this factor
	// under high packet rates (e.g. 50k+ packets/sec across all clients).
	rateShardCount = 16
)

// rateShard holds one shard of the rate limiter with its own mutex.
type rateShard struct {
	mu  sync.Mutex
	buf [2]map[netkey.RateKey]int // double-buffer: [0]=active, [1]=stale
}

// rateShardsArray is the global array of rate limiter shards.
// Accessed via the shard() method which hashes the key to pick a shard.
type rateShardsArray [rateShardCount]*rateShard

func newRateShardsArray() *rateShardsArray {
	var arr rateShardsArray
	for i := range arr {
		arr[i] = &rateShard{
			buf: [2]map[netkey.RateKey]int{make(map[netkey.RateKey]int), make(map[netkey.RateKey]int)},
		}
	}
	return &arr
}

// shard returns the shard index for a given rateKey.
// Uses FNV-1a hash over all 16 IP bytes for even distribution across IPv4/IPv6.
func (r *rateShardsArray) shard(key netkey.RateKey) *rateShard {
	// FNV-1a hash (offset basis + prime per byte)
	h := uint32(2166136261)
	for _, b := range key.IP {
		h ^= uint32(b)
		h *= 16777619
	}
	return r[h%rateShardCount]
}

// checkRate returns true if the address has not exceeded the packet rate limit.
func (s *Server) checkRate(addr *net.UDPAddr) bool {
	key := netkey.AddrToRateKey(addr)
	shard := s.rateShards.shard(key)
	shard.mu.Lock()
	shard.buf[0][key]++
	ok := shard.buf[0][key] <= rateLimit
	shard.mu.Unlock()
	return ok
}

// rateLimitLoop resets the per-client packet counter every second
// using a double-buffer swap per shard: swap pointers under lock (O(1)), then
// clear the stale buffer to reuse its memory allocation.
func (s *Server) rateLimitLoop(ctx context.Context) {
	defer s.rateTick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.rateTick.C:
			for i := 0; i < rateShardCount; i++ {
				shard := s.rateShards[i]
				shard.mu.Lock()
				shard.buf[0], shard.buf[1] = shard.buf[1], shard.buf[0]
				// Clear the stale buffer instead of replacing it with a new map.
				// This reuses the existing map memory, avoiding GC pressure from
				// creating a new map every second under high connection counts.
				clear(shard.buf[1])
				shard.mu.Unlock()
			}
		}
	}
}
