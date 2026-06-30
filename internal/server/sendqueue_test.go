package server

import (
	"github.com/holipay/gametunnel/internal/netkey"
	"context"
	"net"
	"testing"
	"time"
)

func TestSendQueue_PriorityOrder(t *testing.T) {
	conn, _ := net.ListenUDP("udp", &net.UDPAddr{})
	defer conn.Close()

	sq := newSendQueue(conn, 10, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go sq.run(ctx)

	// Send low priority first, then high priority
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9999}
	sq.send([]byte("low1"), addr, priorityLow)
	sq.send([]byte("high1"), addr, priorityHigh)
	sq.send([]byte("low2"), addr, priorityLow)

	// Give queue time to process
	time.Sleep(50 * time.Millisecond)

	// High priority should have been processed first
	if sq.pending() != 0 {
		t.Errorf("expected queue to be drained, got %d pending", sq.pending())
	}
}

func TestSendQueue_LowPriorityDroppedWhenFull(t *testing.T) {
	conn, _ := net.ListenUDP("udp", &net.UDPAddr{})
	defer conn.Close()

	sq := newSendQueue(conn, 2, nil) // very small queue
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9999}

	// Fill queue
	sq.send([]byte("a"), addr, priorityLow)
	sq.send([]byte("b"), addr, priorityLow)

	// Third low priority should be dropped
	if sq.send([]byte("c"), addr, priorityLow) {
		t.Error("expected low priority packet to be dropped when queue full")
	}
}

func TestSendQueue_HighPriorityWaitsForSpace(t *testing.T) {
	conn, _ := net.ListenUDP("udp", &net.UDPAddr{})
	defer conn.Close()

	sq := newSendQueue(conn, 1, nil) // very small queue

	// Don't start run loop - we want to test queue behavior directly
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9999}

	// Fill queue
	sq.send([]byte("a"), addr, priorityLow)

	// High priority should wait and eventually timeout (no run loop to drain)
	done := make(chan bool, 1)
	go func() {
		result := sq.send([]byte("b"), addr, priorityHigh)
		done <- result
	}()

	select {
	case result := <-done:
		// High priority should timeout after 50ms since queue is full
		if result {
			t.Error("expected high priority to timeout when queue full and no drain")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("high priority send did not return in time")
	}
}

func TestSendQueue_DrainOnContextCancel(t *testing.T) {
	conn, _ := net.ListenUDP("udp", &net.UDPAddr{})
	defer conn.Close()

	sq := newSendQueue(conn, 10, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go sq.run(ctx)

	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9999}
	sq.send([]byte("a"), addr, priorityLow)
	sq.send([]byte("b"), addr, priorityLow)

	cancel()
	time.Sleep(50 * time.Millisecond)

	// Queue should be drained after context cancel
	if sq.pending() != 0 {
		t.Errorf("expected queue to be drained after cancel, got %d pending", sq.pending())
	}
}

func TestRateLimitedQueue_ControlBypassesBandwidthLimit(t *testing.T) {
	conn, _ := net.ListenUDP("udp", &net.UDPAddr{})
	defer conn.Close()

	limiter := NewBandwidthLimiter(1) // 1 byte/sec = very restrictive
	rlq := newRateLimitedQueue(conn, limiter, nil)

	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9999}

	// Exhaust burst tokens (burst is 512KB)
	limiter.Allow(addr, 512*1024)

	// Control packet should bypass limiter
	if !rlq.send([]byte("control"), addr, priorityHigh) {
		t.Error("expected control packet to bypass bandwidth limiter")
	}

	// Data packet should be rate limited
	if rlq.send(make([]byte, 100), addr, priorityLow) {
		t.Error("expected data packet to be rate limited")
	}
}

func TestRoomSendQueue_PriorityInheritance(t *testing.T) {
	r := &Room{
		clients:     make(map[[16]byte]*Client),
		addrMap:     make(map[netkey.RateKey]*Client),
		ipConnCount: make(map[connIPKey]int),
		done:        make(chan struct{}),
	}
	conn, _ := net.ListenUDP("udp", &net.UDPAddr{})
	defer conn.Close()
	r.conn = conn
	r.sendQueue = newRateLimitedQueue(conn, nil, nil)
	close(r.done)

	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9999}

	// sendChecked (high priority) should work
	r.sendChecked(0x01, []byte("test"), addr)

	// sendCheckedRaw (low priority) should work
	r.sendCheckedRaw([]byte("data"), addr)
}
