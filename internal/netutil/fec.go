package netutil

import (
	"encoding/binary"
	"sync"
	"sync/atomic"
	"time"
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
		newParity := make([]byte, len(data))
		copy(newParity, e.parity)
		e.parity = newParity
		e.maxPktLen = len(data)
	}

	// XOR data into running parity
	for i := 0; i < len(data); i++ {
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
	gid := e.groupID.Add(1)
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
	e.pktCount = 0
	e.maxPktLen = 0
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

	// Stats
	recovered atomic.Uint64 // packets recovered via FEC
	dropped   atomic.Uint64 // packets lost beyond recovery
}

type fecGroup struct {
	id       uint32
	size     int
	received map[byte][]byte // index → data (only received packets)
	parity   []byte          // parity packet data
	parityOK bool            // true if parity packet received
	created  time.Time       // when this group was created (for age-based cleanup)
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

	g := d.getOrCreateGroup(groupID)
	if g.received[seq] != nil {
		return nil // duplicate
	}
	g.received[seq] = copyBytes(data)

	return d.tryRecover(g)
}

// ProcessParityPacket processes a received FEC parity packet.
// Returns recovered packets (if any).
func (d *FECDecoder) ProcessParityPacket(groupID uint32, groupSize int, parity []byte) [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()

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
			id:       groupID,
			size:     d.groupSize,
			received: make(map[byte][]byte),
			created:  time.Now(),
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
	if !g.parityOK || len(g.received) < g.size-1 {
		return nil // need parity + at least size-1 data packets
	}

	if len(g.received) == g.size {
		return nil // all packets received, no recovery needed
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
		for j, pkt := range g.received {
			if j == idx {
				continue
			}
			xorBytes(parity, pkt)
		}

		recovered = append(recovered, parity)
		d.recovered.Add(1)
		delete(d.groups, g.id) // group complete
		break
	}

	return recovered
}

// Stats returns FEC recovery statistics.
func (d *FECDecoder) Stats() (recovered, dropped uint64) {
	return d.recovered.Load(), d.dropped.Load()
}

// Close stops the decoder's background cleanup.
func (d *FECDecoder) Close() {
	close(d.done)
	d.cleanTick.Stop()
}

// cleanupLoop periodically removes stale groups.
func (d *FECDecoder) cleanupLoop() {
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
				if len(g.received) < g.size {
					d.dropped.Add(1)
				}
				delete(d.groups, id)
			}
			d.mu.Unlock()
		}
	}
}

// ── Helpers ────────────────────────────────────────────────────

// xorBytes performs dst = dst XOR src (in-place).
func xorBytes(dst, src []byte) {
	n := len(dst)
	if len(src) < n {
		n = len(src)
	}
	for i := 0; i < n; i++ {
		dst[i] ^= src[i]
	}
}

func copyBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	c := make([]byte, len(b))
	copy(c, b)
	return c
}

// IsFECPacket checks if data is an FEC parity packet by looking at
// the groupSize field (byte 4). Valid group sizes are 2-32.
func IsFECPacket(data []byte) bool {
	if len(data) < FECHeaderSize {
		return false
	}
	gs := data[4]
	return gs >= 2 && gs <= 32
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
