package protocol

import (
	"net"
	"sync"
)

// ── Data (relay) ───────────────────────────────────────────────

// DataFlagCompressed was used for LZ4 compression (now removed).
// Reserved for backward compatibility — receivers silently ignore this flag.
const DataFlagCompressed byte = 0x01

// DataFlagHasToken is set in Flags when a 16-byte session token follows the flags byte.
// Only used when both client and server are v1.7+.
const DataFlagHasToken byte = 0x02

// DataFlagHasFEC is set in Flags when a 5-byte FEC header (groupID + seq)
// is appended at the end of the data payload. Used for forward error correction.
// Only used when server version >= 0x0108.
const DataFlagHasFEC byte = 0x04

// FECHeaderSize is the size of the FEC header appended to the data payload.
// Wire format: [groupID(4, LE)] [seq(1)]
const FECHeaderSize = 5

// DataPayload carries a relayed IP packet between client and server.
// Wire format: srcIP(4) + dstIP(4) + flags(1) + data(N)
//
// Flags:
//
//	0x00 = data is uncompressed (legacy compatible)
//	0x01 = reserved (was LZ4-compressed, now unused)
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
	New: func() interface{} {
		return &DataPayload{
			SrcIP: make([]byte, 0, 4),
			DstIP: make([]byte, 0, 4),
		}
	},
}

// PutDataPayload returns a DataPayload to the pool. Callers MUST NOT use the
// object or any of its fields after calling this.
func PutDataPayload(dp *DataPayload) {
	// Reset lengths but keep backing arrays for zero-allocation reuse.
	dp.SrcIP = dp.SrcIP[:0]
	dp.DstIP = dp.DstIP[:0]
	dp.Data = dp.Data[:0]
	dp.Flags = 0
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
	if len(data) > 8 && isNewFormat(data[8]) {
		dp.Flags = data[8]
		offset := 9
		if dp.Flags&DataFlagHasToken != 0 {
			offset += 16
		}
		if offset > len(data) {
			return nil, ErrPacketTooShort
		}
		dataLen := len(data) - offset
		if dp.Flags&DataFlagHasFEC != 0 {
			if dataLen < FECHeaderSize {
				return nil, ErrPacketTooShort
			}
			dataLen -= FECHeaderSize
		}
		pktData := make([]byte, dataLen)
		copy(pktData, data[offset:])
		dp.Data = pktData
	} else {
		pktData := make([]byte, len(data)-8)
		copy(pktData, data[8:])
		dp.Data = pktData
	}
	return dp, nil
}

// UnmarshalDataPooled is like UnmarshalData but reuses a pooled DataPayload
// with zero heap allocations (backing arrays are preserved across calls).
func UnmarshalDataPooled(data []byte) (*DataPayload, error) {
	if len(data) < 8 {
		return nil, ErrPacketTooShort
	}
	dp := dataPayloadPool.Get().(*DataPayload)
	if cap(dp.SrcIP) < 4 {
		dp.SrcIP = make([]byte, 4)
	} else {
		dp.SrcIP = dp.SrcIP[:4]
	}
	if cap(dp.DstIP) < 4 {
		dp.DstIP = make([]byte, 4)
	} else {
		dp.DstIP = dp.DstIP[:4]
	}
	copy(dp.SrcIP, data[0:4])
	copy(dp.DstIP, data[4:8])
	if len(data) > 8 && isNewFormat(data[8]) {
		dp.Flags = data[8]
		offset := 9
		if dp.Flags&DataFlagHasToken != 0 {
			offset += 16
		}
		if offset > len(data) {
			PutDataPayload(dp)
			return nil, ErrPacketTooShort
		}
		rawData := data[offset:]
		if dp.Flags&DataFlagHasFEC != 0 {
			if len(rawData) < FECHeaderSize {
				PutDataPayload(dp)
				return nil, ErrPacketTooShort
			}
			rawData = rawData[:len(rawData)-FECHeaderSize]
		}
		if cap(dp.Data) < len(rawData) {
			dp.Data = make([]byte, len(rawData))
		} else {
			dp.Data = dp.Data[:len(rawData)]
		}
		copy(dp.Data, rawData)
	} else {
		dp.Flags = 0
		dataLen := len(data) - 8
		if cap(dp.Data) < dataLen {
			dp.Data = make([]byte, dataLen)
		} else {
			dp.Data = dp.Data[:dataLen]
		}
		copy(dp.Data, data[8:])
	}
	return dp, nil
}

// isNewFormat returns true if the byte looks like a flags byte (0x00-0x07)
// rather than an IPv4 version nibble (0x45-0x4F).
// This is the backward-compatibility heuristic for detecting old vs new format.
// IMPORTANT: Flags values 0x00-0x07 are valid. Values 0x08-0xFF are reserved to
// avoid collision with old-format IPv4 headers (0x4x-0xFx).
func isNewFormat(b byte) bool {
	return b <= 0x07
}

// IsFECEnabled returns true if the data payload flags indicate FEC header is present.
func IsFECEnabled(flags byte) bool {
	return flags&DataFlagHasFEC != 0
}
