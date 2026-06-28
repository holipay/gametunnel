package server

import (
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

// rateKey is a fixed-size key for rate limiting, avoiding string allocation per packet.
// Uses 16-byte IP to support both IPv4 (as v4-in-v6 mapped) and IPv6 addresses.
type rateKey struct {
	IP   [16]byte
	Port uint16
}

func addrToRateKey(addr *net.UDPAddr) rateKey {
	var k rateKey
	if len(addr.IP) == net.IPv4len {
		// v4-in-v6: 0:0:0:0:0:ffff:a.b.c.d
		k.IP[10] = 0xff
		k.IP[11] = 0xff
		copy(k.IP[12:16], addr.IP)
	} else {
		copy(k.IP[:], addr.IP)
	}
	k.Port = uint16(addr.Port)
	return k
}

// rateShard holds one shard of the rate limiter with its own mutex.
type rateShard struct {
	mu  sync.Mutex
	buf [2]map[rateKey]int // double-buffer: [0]=active, [1]=stale
}

// rateShardsArray is the global array of rate limiter shards.
// Accessed via the shard() method which hashes the key to pick a shard.
type rateShardsArray [rateShardCount]*rateShard

func newRateShardsArray() *rateShardsArray {
	var arr rateShardsArray
	for i := range arr {
		arr[i] = &rateShard{
			buf: [2]map[rateKey]int{make(map[rateKey]int), make(map[rateKey]int)},
		}
	}
	return &arr
}

// shard returns the shard index for a given rateKey.
// Uses a simple hash of the IP bytes to distribute across shards.
func (r *rateShardsArray) shard(key rateKey) *rateShard {
	// Use the last 4 bytes of the IP (IPv4 portion for v4-in-v6) for hashing.
	// This ensures packets from the same IP always go to the same shard.
	h := uint32(key.IP[12]) | uint32(key.IP[13])<<8 | uint32(key.IP[14])<<16 | uint32(key.IP[15])<<24
	return r[h%rateShardCount]
}

// checkRate returns true if the address has not exceeded the packet rate limit.
func (s *Server) checkRate(addr *net.UDPAddr) bool {
	key := addrToRateKey(addr)
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
