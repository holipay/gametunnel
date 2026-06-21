package client

import (
	"testing"
	"time"
)

func TestClientSendLimiter_Allow(t *testing.T) {
	// 100 bytes/sec, 200 byte burst
	l := newClientSendLimiter(100, 200)

	// First send should succeed (full burst)
	if !l.allow(100) {
		t.Error("expected first send to succeed")
	}

	// Second send should succeed (still within burst)
	if !l.allow(100) {
		t.Error("expected second send to succeed")
	}

	// Third send should fail (burst exhausted)
	if l.allow(100) {
		t.Error("expected third send to be rate limited")
	}
}

func TestClientSendLimiter_Refill(t *testing.T) {
	// 100 bytes/sec, 100 byte burst
	l := newClientSendLimiter(100, 100)

	// Exhaust tokens
	if !l.allow(100) {
		t.Fatal("expected first send to succeed")
	}

	// Should fail immediately
	if l.allow(10) {
		t.Error("expected send to be rate limited")
	}

	// Wait for refill
	time.Sleep(200 * time.Millisecond)

	// Should succeed after refill
	if !l.allow(10) {
		t.Error("expected send to succeed after refill")
	}
}

func TestClientSendLimiter_BurstCap(t *testing.T) {
	// 1000 bytes/sec, 100 byte burst (burst < rate)
	l := newClientSendLimiter(1000, 100)

	// Send 100 bytes (burst)
	if !l.allow(100) {
		t.Error("expected burst to succeed")
	}

	// Wait for rate to refill beyond burst
	time.Sleep(200 * time.Millisecond)

	// Should only have 100 tokens (burst cap)
	if !l.allow(100) {
		t.Error("expected 100 bytes to succeed")
	}

	// Should fail
	if l.allow(10) {
		t.Error("expected send to be rate limited")
	}
}

func TestClientSendLimiter_NilSafe(t *testing.T) {
	var l *clientSendLimiter

	// nil limiter should always allow
	if !l.allow(1000) {
		t.Error("expected nil limiter to allow")
	}
}

func TestClientSendLimiter_SmallPacket(t *testing.T) {
	// 10 bytes/sec, 10 byte burst
	l := newClientSendLimiter(10, 10)

	// Small packet should succeed
	if !l.allow(1) {
		t.Error("expected 1-byte send to succeed")
	}

	// Should fail immediately (only 9 tokens left, need 10)
	if l.allow(10) {
		t.Error("expected send to be rate limited")
	}
}
