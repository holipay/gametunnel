package netutil

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// FEC (Forward Error Correction) provides packet loss recovery using
// XOR-based parity. Every groupSize data packets, a parity packet is
// generated that allows recovery of any 1 lost packet in the group.
//
// Algorithm: parity = pkt[0] XOR pkt[1] XOR ... XOR pkt[N-1]
// Recovery:  lost_pkt = parity XOR pkt[i0] XOR pkt[i1] XOR ... (all received)
//
// Wire format of FEC parity packet:
//
//	[4B: groupID] [1B: groupSize] [1B: pad0] [1B: pad1] [1B: pad2] [NB: parity_data]
//
// The 4-byte padding ensures the header is 8 bytes total, aligned for
// efficient XOR operations. The parity_data is the XOR of all data
// packets in the group (padded to the length of the longest packet).

const (
	// DefaultFECGroupSize is the number of data packets per FEC group.
	// Every groupSize packets, one parity packet is sent.
	// With groupSize=8, 1 parity per 8 data = 12.5% overhead.
	DefaultFECGroupSize = 8

	// FECHeaderSize is the fixed header size of an FEC parity packet.
	FECHeaderSize = 8 // groupID(4) + groupSize(1) + pad(3)

	// maxGroupSize is the maximum FEC group size allowed from the wire.
	// The received array is [32][]byte, so groupSize > 32 would panic.
	maxGroupSize = 32
)

// ── FEC Encoder (sender side) ──────────────────────────────────

// FECEncoder generates XOR parity packets for outgoing data.
// It accumulates data packets in a group and emits a parity packet
// when the group is full.
type FECEncoder struct {
	groupSize  int
	groupID    atomic.Uint32 // monotonic group counter

	mu         sync.Mutex
	parity     []byte   // running XOR of all packets in current group
	pktCount   int      // packets accumulated in current group
	maxPktLen  int      // max packet length in current group (for padding)
}

// NewFECEncoder creates a new encoder with the given group size.
// groupSize = 0 uses DefaultFECGroupSize.
func NewFECEncoder(groupSize int) *FECEncoder {
	if groupSize <= 0 {
		groupSize = DefaultFECGroupSize
	}
	return &FECEncoder{
		groupSize: groupSize,
	}
}

// Encode processes a data packet and returns a parity packet if the
// group is complete. Returns nil if more packets are needed.
//
// The data should be the raw packet content (e.g. the marshaled DataPayload
// after srcIP+dstIP+flags prefix). Each call advances the group counter.
func (e *FECEncoder) Encode(data []byte) []byte {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Grow parity buffer if this packet is longer than current
	if len(data) > e.maxPktLen {
		newParity := PktBufGet(len(data))
		copy(newParity, e.parity)
		PktBufPut(e.parity)
		e.parity = newParity
		e.maxPktLen = len(data)
	}

	// XOR data into running parity (8 bytes at a time)
	n := len(data)
	i := 0
	for ; i+8 <= n; i += 8 {
		p := *(*uint64)(unsafe.Pointer(&e.parity[i]))
		d := *(*uint64)(unsafe.Pointer(&data[i]))
		*(*uint64)(unsafe.Pointer(&e.parity[i])) = p ^ d
	}
	for ; i < n; i++ {
		e.parity[i] ^= data[i]
	}
	e.pktCount++

	// If group is full, emit parity packet
	if e.pktCount >= e.groupSize {
		parity := e.buildParityPacket()
		e.resetGroup()
		return parity
	}

	return nil
}

// CurrentGroupInfo returns the current group ID and packet sequence number
// within the group. Used by the sender to embed FEC metadata in data packets
// so the receiver can associate them with the correct group.
func (e *FECEncoder) CurrentGroupInfo() (groupID uint32, seq byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.groupID.Load(), byte(e.pktCount)
}

// Flush forces emission of a parity packet for the current (possibly
// incomplete) group. Call this when there's a long pause in traffic
// to avoid waiting for the group to fill.
func (e *FECEncoder) Flush() []byte {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.pktCount == 0 {
		return nil
	}

	parity := e.buildParityPacket()
	e.resetGroup()
	return parity
}

// buildParityPacket builds the wire-format FEC parity packet.
// Must be called with e.mu held.
func (e *FECEncoder) buildParityPacket() []byte {
	gid := e.groupID.Load()
	buf := make([]byte, FECHeaderSize+e.maxPktLen)
	binary.LittleEndian.PutUint32(buf[0:4], gid)
	buf[4] = byte(e.groupSize)
	buf[5] = 0 // padding
	buf[6] = 0
	buf[7] = 0
	copy(buf[FECHeaderSize:], e.parity)
	return buf
}

// resetGroup resets the encoder for a new group.
// Must be called with e.mu held.
func (e *FECEncoder) resetGroup() {
	e.groupID.Add(1)
	e.pktCount = 0
	e.maxPktLen = 0
	PktBufPut(e.parity)
	e.parity = nil
}

// ── FEC Decoder (receiver side) ────────────────────────────────

// FECDecoder recovers lost packets using XOR parity.
// It buffers incoming data packets by group and uses parity packets
// to fill gaps.
type FECDecoder struct {
	groupSize int

	mu        sync.Mutex
	groups    map[uint32]*fecGroup // active groups by groupID
	cleanTick *time.Ticker
	done      chan struct{}
	wg        sync.WaitGroup // tracks cleanupLoop goroutine
	closed    bool           // true after Close() is called

	// Stats
	recovered atomic.Uint64 // packets recovered via FEC
	dropped   atomic.Uint64 // packets lost beyond recovery
}

type fecGroup struct {
	id        uint32
	size      int
	received  [32][]byte // index → data (only received packets)
	recvCount int        // number of non-nil entries in received
	parity    []byte     // parity packet data
	parityOK  bool       // true if parity packet received
	created   time.Time  // when this group was created (for age-based cleanup)
}

// NewFECDecoder creates a new decoder with the given group size.
func NewFECDecoder(groupSize int) *FECDecoder {
	if groupSize <= 0 {
		groupSize = DefaultFECGroupSize
	}
	d := &FECDecoder{
		groupSize: groupSize,
		groups:    make(map[uint32]*fecGroup),
		cleanTick: time.NewTicker(5 * time.Second),
		done:      make(chan struct{}),
	}
	d.wg.Add(1)
	go d.cleanupLoop()
	return d
}

// ProcessDataPacket processes a received data packet.
// seq is the packet's sequence number within its FEC group.
// groupID is the FEC group this packet belongs to.
// data is the packet payload (the part that was XOR'd by the encoder).
//
// Returns recovered packets (if any) after this packet fills a gap.
func (d *FECDecoder) ProcessDataPacket(groupID uint32, seq byte, data []byte) [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		return nil
	}

	g := d.getOrCreateGroup(groupID)
	if int(seq) >= len(g.received) || int(seq) >= g.size || g.received[seq] != nil {
		return nil // duplicate or out of range
	}
	g.received[seq] = copyBytes(data)
	g.recvCount++

	return d.tryRecover(g)
}

// ProcessParityPacket processes a received FEC parity packet.
// Returns recovered packets (if any).
func (d *FECDecoder) ProcessParityPacket(groupID uint32, groupSize int, parity []byte) [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		return nil
	}

	if groupSize > maxGroupSize {
		d.dropped.Add(1)
		return nil
	}

	g := d.getOrCreateGroup(groupID)
	g.size = groupSize
	g.parity = copyBytes(parity)
	g.parityOK = true

	return d.tryRecover(g)
}

// getOrCreateGroup returns the group for the given ID, creating if needed.
// Must be called with d.mu held.
func (d *FECDecoder) getOrCreateGroup(groupID uint32) *fecGroup {
	g, ok := d.groups[groupID]
	if !ok {
		g = &fecGroup{
			id:      groupID,
			size:    d.groupSize,
			created: time.Now(),
		}
		d.groups[groupID] = g
	}
	return g
}

// tryRecover attempts to recover lost packets in a group.
// Recovery is possible when:
//   - Parity packet is received
//   - Exactly 1 packet is missing (groupSize - 1 received)
//
// Returns recovered packet data (nil if no recovery possible).
// Must be called with d.mu held.
func (d *FECDecoder) tryRecover(g *fecGroup) [][]byte {
	if !g.parityOK || g.recvCount < g.size-1 {
		return nil // need parity + at least size-1 data packets
	}

	if g.recvCount == g.size {
		releaseGroupBuffers(g)
		delete(d.groups, g.id) // all received, no recovery needed
		return nil
	}

	// Exactly 1 missing — recover it
	// Find the missing index
	var recovered [][]byte
	for i := 0; i < g.size; i++ {
		idx := byte(i)
		if g.received[idx] != nil {
			continue
		}

		// Recover: lost = parity XOR all_received
		parity := copyBytes(g.parity)
		for j := 0; j < g.size; j++ {
			if byte(j) == idx {
				continue
			}
			xorBytes(parity, g.received[j])
		}

		recovered = append(recovered, parity)
		d.recovered.Add(1)
		releaseGroupBuffers(g)
		delete(d.groups, g.id) // group complete
		break
	}

	return recovered
}

// Stats returns FEC recovery statistics.
func (d *FECDecoder) Stats() (recovered, dropped uint64) {
	return d.recovered.Load(), d.dropped.Load()
}

// Close stops the decoder's background cleanup and waits for it to exit.
func (d *FECDecoder) Close() {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	close(d.done)
	d.cleanTick.Stop()
	d.wg.Wait()
}

// cleanupLoop periodically removes stale groups.
func (d *FECDecoder) cleanupLoop() {
	defer d.wg.Done()
	for {
		select {
		case <-d.done:
			return
		case <-d.cleanTick.C:
			d.mu.Lock()
			now := time.Now()
			for id, g := range d.groups {
				if now.Sub(g.created) < 10*time.Second {
					continue // group is still active
				}
				// Group expired — count as dropped if incomplete
				if g.recvCount < g.size {
					d.dropped.Add(1)
				}
				releaseGroupBuffers(g)
				delete(d.groups, id)
			}
			d.mu.Unlock()
		}
	}
}

// ── Helpers ────────────────────────────────────────────────────

// xorBytes performs dst = dst XOR src (in-place).
// Processes 8 bytes at a time using uint64 for ~8x throughput on amd64.
func xorBytes(dst, src []byte) {
	n := len(dst)
	if len(src) < n {
		n = len(src)
	}
	// Process 8 bytes at a time
	i := 0
	for ; i+8 <= n; i += 8 {
		d := *(*uint64)(unsafe.Pointer(&dst[i]))
		s := *(*uint64)(unsafe.Pointer(&src[i]))
		*(*uint64)(unsafe.Pointer(&dst[i])) = d ^ s
	}
	// Handle remaining bytes
	for ; i < n; i++ {
		dst[i] ^= src[i]
	}
}

func copyBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	c := PktBufGet(len(b))
	copy(c, b)
	return c[:len(b)]
}

// releaseGroupBuffers returns all pooled buffers in a fecGroup.
func releaseGroupBuffers(g *fecGroup) {
	for i := range g.received {
		if g.received[i] != nil {
			PktBufPut(g.received[i])
			g.received[i] = nil
		}
	}
	if g.parity != nil {
		PktBufPut(g.parity)
		g.parity = nil
	}
}

// IsFECPacket checks if data is an FEC parity packet.
// Heuristic: bytes 4 (groupSize ∈ [2,32]) and bytes 5-7 (padding, always 0).
func IsFECPacket(data []byte) bool {
	if len(data) < FECHeaderSize {
		return false
	}
	gs := data[4]
	return gs >= 2 && gs <= 32 && data[5] == 0 && data[6] == 0 && data[7] == 0
}

// ParseFECHeader extracts groupID and groupSize from an FEC parity packet.
func ParseFECHeader(data []byte) (groupID uint32, groupSize int, err error) {
	if len(data) < FECHeaderSize {
		return 0, 0, ErrPacketTooShort
	}
	groupID = binary.LittleEndian.Uint32(data[0:4])
	groupSize = int(data[4])
	return groupID, groupSize, nil
}

// ParseFECParity extracts the parity data from an FEC parity packet.
func ParseFECParity(data []byte) []byte {
	if len(data) <= FECHeaderSize {
		return nil
	}
	return data[FECHeaderSize:]
}

// ErrPacketTooShort is returned when a packet is too short to parse.
var ErrPacketTooShort = &FECError{"packet too short"}

type FECError struct {
	msg string
}

func (e *FECError) Error() string {
	return e.msg
}
