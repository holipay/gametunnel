package protocol

import (
	"net"
	"sync"
)

// ── Data (relay) ───────────────────────────────────────────────

// DataFlagCompressed is set in Flags when Data is LZ4-compressed.
const DataFlagCompressed byte = 0x01

// DataPayload carries a relayed IP packet between client and server.
// Wire format: srcIP(4) + dstIP(4) + flags(1) + data(N)
//
// Flags:
//
//	0x01 = data is LZ4-compressed (decompress before writing to TUN)
//	0x00 = data is uncompressed (legacy compatible)
//
// Backward compatibility: old clients send 8+N bytes (no flags). The
// Unmarshal functions detect this by checking if len(data) > 8 and the
// first byte after dstIP looks like a valid flags value vs. a valid
// IP packet start (IPv4 version nibble = 0x4).
type DataPayload struct {
	SrcIP  net.IP
	DstIP  net.IP
	Flags  byte   // DataFlagCompressed etc.
	Data   []byte
}

// dataPayloadPool reuses DataPayload objects to reduce GC pressure on the
// hot path (every game data packet goes through UnmarshalData).
var dataPayloadPool = sync.Pool{
	New: func() interface{} { return &DataPayload{} },
}

// PutDataPayload returns a DataPayload to the pool. Callers MUST NOT use the
// object or any of its fields after calling this.
func PutDataPayload(dp *DataPayload) {
	// Clear references to allow GC of underlying buffers
	dp.SrcIP = nil
	dp.DstIP = nil
	dp.Data = nil
	dataPayloadPool.Put(dp)
}

func (d *DataPayload) Marshal() []byte {
	src := d.SrcIP.To4()
	dst := d.DstIP.To4()
	if src == nil || dst == nil {
		return nil
	}
	buf := make([]byte, 9+len(d.Data))
	copy(buf[0:4], src)
	copy(buf[4:8], dst)
	buf[8] = d.Flags
	copy(buf[9:], d.Data)
	return buf
}

// MarshalSize returns the encoded size of this DataPayload.
func (d *DataPayload) MarshalSize() int {
	return 9 + len(d.Data)
}

// MarshalTo writes the encoded payload into dst (zero-copy).
// Returns number of bytes written. Caller must ensure len(dst) >= MarshalSize().
func (d *DataPayload) MarshalTo(dst []byte) int {
	src := d.SrcIP.To4()
	dstIP := d.DstIP.To4()
	if src == nil || dstIP == nil || len(dst) < 9 {
		return 0
	}
	copy(dst[0:4], src)
	copy(dst[4:8], dstIP)
	dst[8] = d.Flags
	return 9 + copy(dst[9:], d.Data)
}

func UnmarshalData(data []byte) (*DataPayload, error) {
	if len(data) < 8 {
		return nil, ErrPacketTooShort
	}
	dp := &DataPayload{
		SrcIP: net.IP(append([]byte(nil), data[0:4]...)),
		DstIP: net.IP(append([]byte(nil), data[4:8]...)),
	}
	// Backward compatibility: detect old format (8+N) vs new format (9+N).
	// Old packets: byte[8] is the first byte of an IPv4 packet → 0x45-0x4F.
	// New packets: byte[8] is flags → 0x00 or 0x01.
	if len(data) > 8 && isNewFormat(data[8]) {
		dp.Flags = data[8]
		pktData := make([]byte, len(data)-9)
		copy(pktData, data[9:])
		dp.Data = pktData
	} else {
		pktData := make([]byte, len(data)-8)
		copy(pktData, data[8:])
		dp.Data = pktData
	}
	return dp, nil
}

// UnmarshalDataPooled is like UnmarshalData but reuses a pooled DataPayload.
func UnmarshalDataPooled(data []byte) (*DataPayload, error) {
	if len(data) < 8 {
		return nil, ErrPacketTooShort
	}
	dp := dataPayloadPool.Get().(*DataPayload)
	dp.SrcIP = append(dp.SrcIP[:0], data[0:4]...)
	dp.DstIP = append(dp.DstIP[:0], data[4:8]...)
	if len(data) > 8 && isNewFormat(data[8]) {
		dp.Flags = data[8]
		dp.Data = append(dp.Data[:0], data[9:]...)
	} else {
		dp.Flags = 0
		dp.Data = append(dp.Data[:0], data[8:]...)
	}
	return dp, nil
}

// isNewFormat returns true if the byte looks like a flags byte (0x00 or 0x01)
// rather than an IPv4 version nibble (0x45-0x4F).
// This is the backward-compatibility heuristic for detecting old vs new format.
func isNewFormat(b byte) bool {
	return b <= 0x01
}

// IsCompressed returns true if the data payload flags indicate LZ4 compression.
func IsCompressed(flags byte) bool {
	return flags&DataFlagCompressed != 0
}
