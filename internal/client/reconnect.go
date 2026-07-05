package client

import (
	"context"
	"math/rand"
	"net"
	"time"
)

// backoffState tracks reconnect backoff state with exponential backoff.
type backoffState struct {
	attempt    int
	maxRetries int // 0 = never stop
}

// nextDelay returns the delay before the next reconnect attempt.
// Returns -1 if reconnecting should stop.
func (b *backoffState) nextDelay(wasConnected bool) time.Duration {
	b.attempt++

	if b.maxRetries > 0 && b.attempt > b.maxRetries {
		return -1
	}

	// Fast reconnect: was connected and lost connection — quick retry
	if wasConnected && b.attempt <= 3 {
		return 500*time.Millisecond + jitter(100*time.Millisecond)
	}

	// Exponential backoff: 2s, 4s, 8s, 16s, 32s, 60s (cap)
	delay := time.Duration(1<<(min(b.attempt, 5))) * time.Second
	if delay > 60*time.Second {
		delay = 60 * time.Second
	}
	return delay + jitter(delay/5)
}

func jitter(maxJitter time.Duration) time.Duration {
	if maxJitter <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(maxJitter)*2)) - maxJitter
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// IsNetworkAvailable checks if the network is likely available by
// attempting a quick DNS lookup.
func IsNetworkAvailable(serverAddr string) bool {
	host, _, err := net.SplitHostPort(serverAddr)
	if err != nil {
		return true
	}
	if net.ParseIP(host) != nil {
		return true
	}
	resolver := net.Resolver{PreferGo: true}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = resolver.LookupHost(ctx, host)
	return err == nil
}
