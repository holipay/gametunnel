package server

import (
	"context"
	"log"
	"net"
	"sync"
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

// batchBufSize is the maximum number of packets drained per batch.
// Reduces syscall overhead by coalescing multiple channel reads.
const batchBufSize = 64

// sendQueue is a bounded, priority-aware send queue backed by a single UDP socket.
// High-priority packets are always sent before low-priority ones.
// When the queue is full, low-priority packets are dropped first.
// Uses batch draining to reduce syscall overhead under high packet rates.
type sendQueue struct {
	conn    *net.UDPConn
	ch      chan sendEntry
	maxSize int
	tcpWrite func(addr *net.UDPAddr, data []byte) bool // optional TCP bridge routing
}

// newSendQueue creates a send queue with the given capacity.
// tcpWrite is an optional callback for routing packets to TCP bridge clients.
func newSendQueue(conn *net.UDPConn, maxSize int, tcpWrite func(addr *net.UDPAddr, data []byte) bool) *sendQueue {
	return &sendQueue{
		conn:     conn,
		ch:       make(chan sendEntry, maxSize),
		maxSize:  maxSize,
		tcpWrite: tcpWrite,
	}
}

// sendTimerPool reuses timers for high-priority sends to avoid per-call allocation.
var sendTimerPool = sync.Pool{
	New: func() interface{} { return time.NewTimer(50 * time.Millisecond) },
}

// send enqueues a packet. Returns false if dropped due to queue full.
// For high-priority packets, blocks up to 50ms waiting for space.
func (sq *sendQueue) send(data []byte, addr *net.UDPAddr, priority sendPriority) bool {
	e := sendEntry{data: data, addr: addr, priority: priority}

	if priority == priorityHigh {
		// High priority: wait briefly for space
		timer := sendTimerPool.Get().(*time.Timer)
		timer.Reset(50 * time.Millisecond)
		select {
		case sq.ch <- e:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			sendTimerPool.Put(timer)
			return true
		case <-timer.C:
			// Timer fired and channel drained — safe to reuse.
			timer.Reset(50 * time.Millisecond)
			sendTimerPool.Put(timer)
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
// Uses batch draining: drains up to batchBufSize high-priority packets first,
// then up to batchBufSize low-priority packets, reducing channel select overhead.
func (sq *sendQueue) run(ctx context.Context) {
	var batch [batchBufSize]sendEntry
	var deferredLow [batchBufSize]sendEntry // low-priority packets found during high-priority drain
	deferredCount := 0

	for {
		select {
		case <-ctx.Done():
			sq.drain()
			return
		case e := <-sq.ch:
			if e.priority == priorityHigh {
				// Send this high-priority packet immediately
				sq.writeUDP(e.data, e.addr)
				// Drain additional high-priority packets, saving any low-priority ones
				n := 0
			DrainHigh:
				for n < batchBufSize {
					select {
					case batch[n] = <-sq.ch:
						if batch[n].priority != priorityHigh {
							// Defer low-priority — don't send during high-priority drain
							if deferredCount < batchBufSize {
								deferredLow[deferredCount] = batch[n]
								deferredCount++
							}
							continue DrainHigh
						}
						n++
					default:
						break DrainHigh
					}
				}
				for i := 0; i < n; i++ {
					sq.writeUDP(batch[i].data, batch[i].addr)
				}
				// Send deferred low-priority packets after all high-priority are done
				for i := 0; i < deferredCount; i++ {
					sq.writeUDP(deferredLow[i].data, deferredLow[i].addr)
				}
				clear(deferredLow[:deferredCount])
				deferredCount = 0
			} else {
				// Low priority: batch drain to reduce per-packet overhead
				batch[0] = e
				n := 1
			DrainLow:
				for n < batchBufSize {
					select {
					case batch[n] = <-sq.ch:
						if batch[n].priority != priorityLow {
							sq.writeUDP(batch[n].data, batch[n].addr)
							continue DrainLow
						}
						n++
					default:
						break DrainLow
					}
				}
				for i := 0; i < n; i++ {
					sq.writeUDP(batch[i].data, batch[i].addr)
				}
			}
		}
	}
}

// writeUDP sends a single UDP packet.
// If the destination is a TCP bridge client, routes via TCP instead.
func (sq *sendQueue) writeUDP(data []byte, addr *net.UDPAddr) {
	if sq.tcpWrite != nil && sq.tcpWrite(addr, data) {
		return
	}
	if _, err := sq.conn.WriteToUDP(data, addr); err != nil {
		log.Printf("[server] send error: %v", err)
	}
}

// drain sends all remaining packets in the queue (best-effort, non-blocking).
func (sq *sendQueue) drain() {
	for {
		select {
		case e := <-sq.ch:
			sq.writeUDP(e.data, e.addr)
		default:
			return
		}
	}
}

// pending returns the number of queued packets.
func (sq *sendQueue) pending() int {
	return len(sq.ch)
}

// ── Rate Limiter Integration ────────────────────────────────

// rateLimitedQueue wraps a sendQueue with a bandwidth limiter check.
// Packets are dropped if the bandwidth limiter rejects them.
type rateLimitedQueue struct {
	sq      *sendQueue
	limiter *BandwidthLimiter
}

func newRateLimitedQueue(conn *net.UDPConn, limiter *BandwidthLimiter, tcpWrite func(addr *net.UDPAddr, data []byte) bool) *rateLimitedQueue {
	return &rateLimitedQueue{
		sq:      newSendQueue(conn, 8192, tcpWrite),
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
