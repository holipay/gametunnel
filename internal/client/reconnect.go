package client

import (
	"context"
	"math/rand"
	"net"
	"strings"
	"time"
)

// DisconnectReason classifies why the connection was lost.
// The reconnect strategy is chosen based on this reason.
type DisconnectReason int

const (
	// DisconnectReasonUnknown — generic error, use default strategy.
	DisconnectReasonUnknown DisconnectReason = iota

	// DisconnectReasonNetworkGlitch — server was connected, then keepalive
	// timeout or send failure. Likely a transient network issue (WiFi↔4G,
	// brief outage). Fast reconnect is appropriate.
	DisconnectReasonNetworkGlitch

	// DisconnectReasonServerUnreachable — can't establish UDP connection
	// to server at all. Server may be down or network is broken.
	// Use exponential backoff.
	DisconnectReasonServerUnreachable

	// DisconnectReasonDNSFailure — server hostname can't be resolved.
	// Likely a DNS issue or wrong hostname. Longer backoff.
	DisconnectReasonDNSFailure

	// DisconnectReasonServerFull — server rejected with "room full".
	// Retrying won't help until a slot opens. Very long backoff.
	DisconnectReasonServerFull

	// DisconnectReasonServerShutdown — server is shutting down gracefully.
	// Moderate backoff, server may restart soon.
	DisconnectReasonServerShutdown

	// DisconnectReasonFatal — non-recoverable error (wrong password,
	// version mismatch). Stop reconnecting entirely.
	DisconnectReasonFatal
)

// ClassifyError determines the disconnect reason from an error message.
func ClassifyError(err error) DisconnectReason {
	if err == nil {
		return DisconnectReasonUnknown
	}
	msg := strings.ToLower(err.Error())

	switch {
	case strings.Contains(msg, "password") || strings.Contains(msg, "密码"):
		return DisconnectReasonFatal
	case strings.Contains(msg, "version") || strings.Contains(msg, "版本"):
		return DisconnectReasonFatal
	case strings.Contains(msg, "room full") || strings.Contains(msg, "房间已满"):
		return DisconnectReasonServerFull
	case strings.Contains(msg, "shutdown") || strings.Contains(msg, "关闭"):
		return DisconnectReasonServerShutdown
	case strings.Contains(msg, "dns") || strings.Contains(msg, "no such host") || strings.Contains(msg, "lookup"):
		return DisconnectReasonDNSFailure
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "i/o timeout"):
		return DisconnectReasonServerUnreachable
	case strings.Contains(msg, "connection refused") || strings.Contains(msg, "connect:"):
		return DisconnectReasonServerUnreachable
	case strings.Contains(msg, "network") || strings.Contains(msg, "unreachable"):
		return DisconnectReasonNetworkGlitch
	default:
		return DisconnectReasonUnknown
	}
}

// BackoffConfig defines backoff parameters for a disconnect reason.
type BackoffConfig struct {
	BaseDelay  time.Duration // initial delay
	MaxDelay   time.Duration // maximum delay cap
	Multiplier float64       // multiplier per retry (exponential backoff)
	FastRetry  int           // number of fast retries before applying backoff
	StopAfter  int           // stop after this many attempts (0 = never stop)
}

// backoffConfigs defines the backoff strategy for each disconnect reason.
var backoffConfigs = map[DisconnectReason]BackoffConfig{
	// Network glitch: 500ms, 750ms, 1.1s, 1.7s, 2.5s, 3.8s, 5.7s, 8.5s, 12.8s, 19.2s, 30s (cap)
	// Fast reconnect for transient issues.
	DisconnectReasonNetworkGlitch: {
		BaseDelay:  500 * time.Millisecond,
		MaxDelay:   30 * time.Second,
		Multiplier: 1.5,
		FastRetry:  3, // first 3 retries are immediate (500ms)
		StopAfter:  0, // never stop
	},

	// Server unreachable: 2s, 4s, 8s, 16s, 32s, 60s (cap)
	// Server might be temporarily down.
	DisconnectReasonServerUnreachable: {
		BaseDelay:  2 * time.Second,
		MaxDelay:   60 * time.Second,
		Multiplier: 2.0,
		FastRetry:  2,
		StopAfter:  0,
	},

	// DNS failure: 5s, 15s, 45s, 60s (cap)
	// DNS issues may take longer to resolve.
	DisconnectReasonDNSFailure: {
		BaseDelay:  5 * time.Second,
		MaxDelay:   60 * time.Second,
		Multiplier: 3.0,
		FastRetry:  0,
		StopAfter:  0,
	},

	// Server full: 10s, 20s, 40s, 60s (cap)
	// Need to wait for a slot to open.
	DisconnectReasonServerFull: {
		BaseDelay:  10 * time.Second,
		MaxDelay:   60 * time.Second,
		Multiplier: 2.0,
		FastRetry:  0,
		StopAfter:  0,
	},

	// Server shutdown: 3s, 6s, 12s, 24s, 48s, 60s (cap)
	// Server may restart soon.
	DisconnectReasonServerShutdown: {
		BaseDelay:  3 * time.Second,
		MaxDelay:   60 * time.Second,
		Multiplier: 2.0,
		FastRetry:  1,
		StopAfter:  0,
	},

	// Fatal: no retries.
	DisconnectReasonFatal: {
		BaseDelay:  0,
		MaxDelay:   0,
		Multiplier: 0,
		FastRetry:  0,
		StopAfter:  1, // stop immediately
	},

	// Unknown: moderate backoff.
	DisconnectReasonUnknown: {
		BaseDelay:  2 * time.Second,
		MaxDelay:   60 * time.Second,
		Multiplier: 1.5,
		FastRetry:  3,
		StopAfter:  0,
	},
}

// SmartBackoff calculates the next reconnect delay based on the disconnect
// reason and the number of attempts so far.
//
// For "was connected then lost" scenarios, the first few attempts use a
// fast reconnect strategy (short delay) before falling back to normal backoff.
type SmartBackoff struct {
	reason       DisconnectReason
	attempt      int
	config       BackoffConfig
	wasConnected bool // true if we were previously connected (vs never connected)
}

// NewSmartBackoff creates a backoff calculator for the given disconnect reason.
// wasConnected should be true if the client was previously connected and lost
// the connection (as opposed to failing on initial connect).
func NewSmartBackoff(reason DisconnectReason, wasConnected bool) *SmartBackoff {
	cfg, ok := backoffConfigs[reason]
	if !ok {
		cfg = backoffConfigs[DisconnectReasonUnknown]
	}
	return &SmartBackoff{
		reason:       reason,
		config:       cfg,
		wasConnected: wasConnected,
	}
}

// Next returns the delay before the next reconnect attempt.
// Returns -1 if reconnecting should stop.
func (b *SmartBackoff) Next() time.Duration {
	b.attempt++

	// Check if we should stop
	if b.config.StopAfter > 0 && b.attempt >= b.config.StopAfter {
		return -1
	}

	// Fast retry phase: use minimal delay for the first N attempts
	if b.attempt <= b.config.FastRetry {
		if b.wasConnected {
			// Was connected — even faster: 500ms for first 3 attempts
			return 500*time.Millisecond + jitter(100*time.Millisecond)
		}
		return b.config.BaseDelay + jitter(b.config.BaseDelay/5)
	}

	// Exponential backoff phase
	delay := float64(b.config.BaseDelay)
	for i := 1; i < b.attempt-b.config.FastRetry; i++ {
		delay *= b.config.Multiplier
	}

	result := time.Duration(delay)
	if result > b.config.MaxDelay {
		result = b.config.MaxDelay
	}

	return result + jitter(result/10)
}

// jitter returns a random duration in [-maxJitter, +maxJitter].
func jitter(maxJitter time.Duration) time.Duration {
	if maxJitter <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(maxJitter)*2)) - maxJitter
}

// ── Network Availability Check ─────────────────────────────────

// IsNetworkAvailable checks if the network is likely available by
// attempting a quick DNS lookup. Returns true if the network seems
// usable, false otherwise.
//
// This is a lightweight check (~2s timeout) to avoid wasting
// reconnect attempts when the network is clearly down.
func IsNetworkAvailable(serverAddr string) bool {
	host, _, err := net.SplitHostPort(serverAddr)
	if err != nil {
		return true // can't parse — let reconnect handle it
	}

	// If it's an IP address, skip DNS check
	if net.ParseIP(host) != nil {
		return true
	}

	// DNS lookup with short timeout
	resolver := net.Resolver{PreferGo: true}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err = resolver.LookupHost(ctx, host)
	return err == nil
}


