package server

import (
	"context"
	"log"
	"net"
	"time"
)

// sendPriority defines packet priority levels.
// Higher priority packets are sent first.
type sendPriority int

const (
	priorityLow  sendPriority = 0 // relay data, bulk traffic
	priorityHigh sendPriority = 1 // control: PeerInfo, Ping, HolePunch, Kick, AssignIP
)

// sendEntry is a single packet in the send queue.
type sendEntry struct {
	data     []byte
	addr     *net.UDPAddr
	priority sendPriority
}

// sendQueue is a bounded, priority-aware send queue backed by a single UDP socket.
// High-priority packets are always sent before low-priority ones.
// When the queue is full, low-priority packets are dropped first.
type sendQueue struct {
	conn    *net.UDPConn
	ch      chan sendEntry
	maxSize int
}

// newSendQueue creates a send queue with the given capacity.
func newSendQueue(conn *net.UDPConn, maxSize int) *sendQueue {
	return &sendQueue{
		conn:    conn,
		ch:      make(chan sendEntry, maxSize),
		maxSize: maxSize,
	}
}

// send enqueues a packet. Returns false if dropped due to queue full.
// For high-priority packets, blocks up to 50ms waiting for space.
func (sq *sendQueue) send(data []byte, addr *net.UDPAddr, priority sendPriority) bool {
	e := sendEntry{data: data, addr: addr, priority: priority}

	if priority == priorityHigh {
		// High priority: wait briefly for space
		timer := time.NewTimer(50 * time.Millisecond)
		defer timer.Stop()
		select {
		case sq.ch <- e:
			return true
		case <-timer.C:
			return false
		}
	}

	// Low priority: drop immediately if full
	select {
	case sq.ch <- e:
		return true
	default:
		return false
	}
}

// run drains the queue and sends packets. Blocks until ctx is cancelled.
func (sq *sendQueue) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			// Drain remaining packets
			sq.drain()
			return
		case e := <-sq.ch:
			if _, err := sq.conn.WriteToUDP(e.data, e.addr); err != nil {
				log.Printf("[server] send error: %v", err)
			}
		}
	}
}

// drain sends all remaining packets in the queue (best-effort, non-blocking).
func (sq *sendQueue) drain() {
	for {
		select {
		case e := <-sq.ch:
			sq.conn.WriteToUDP(e.data, e.addr) //nolint:errcheck
		default:
			return
		}
	}
}

// pending returns the number of queued packets.
func (sq *sendQueue) pending() int {
	return len(sq.ch)
}

// serverSendQueue wraps sendQueue for the Server struct.
type serverSendQueue struct {
	sq *sendQueue
}

func newServerSendQueue(conn *net.UDPConn) *serverSendQueue {
	return &serverSendQueue{sq: newSendQueue(conn, 4096)}
}

func (ssq *serverSendQueue) sendHigh(data []byte, addr *net.UDPAddr) bool {
	return ssq.sq.send(data, addr, priorityHigh)
}

func (ssq *serverSendQueue) sendLow(data []byte, addr *net.UDPAddr) bool {
	return ssq.sq.send(data, addr, priorityLow)
}

// roomSendQueue wraps sendQueue for Room-level sends.
type roomSendQueue struct {
	sq *sendQueue
}

func newRoomSendQueue(conn *net.UDPConn) *roomSendQueue {
	return &roomSendQueue{sq: newSendQueue(conn, 4096)}
}

func (rsq *roomSendQueue) sendHigh(data []byte, addr *net.UDPAddr) bool {
	return rsq.sq.send(data, addr, priorityHigh)
}

func (rsq *roomSendQueue) sendLow(data []byte, addr *net.UDPAddr) bool {
	return rsq.sq.send(data, addr, priorityLow)
}

// ── Rate Limiter Integration ────────────────────────────────

// rateLimitedQueue wraps a sendQueue with a bandwidth limiter check.
// Packets are dropped if the bandwidth limiter rejects them.
type rateLimitedQueue struct {
	sq        *sendQueue
	limiter   *BandwidthLimiter
}

func newRateLimitedQueue(conn *net.UDPConn, limiter *BandwidthLimiter) *rateLimitedQueue {
	return &rateLimitedQueue{
		sq:      newSendQueue(conn, 4096),
		limiter: limiter,
	}
}

// send enqueues a packet after checking the bandwidth limiter.
// Control packets bypass the limiter.
func (rlq *rateLimitedQueue) send(data []byte, addr *net.UDPAddr, priority sendPriority) bool {
	if priority == priorityHigh || rlq.limiter == nil || rlq.limiter.Allow(addr, len(data)) {
		return rlq.sq.send(data, addr, priority)
	}
	return false
}

func (rlq *rateLimitedQueue) run(ctx context.Context) {
	rlq.sq.run(ctx)
}
