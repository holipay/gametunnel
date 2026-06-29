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

// DataFlagHasFEC was used for FEC header (now removed).
// Reserved for backward compatibility — receivers silently ignore this flag.
const DataFlagHasFEC byte = 0x04

// DataFormatVersion is the explicit format version byte written after dstIP
// in the DataPayload wire format. Replaces the old isNewFormat heuristic.
// 0x01 = current format: srcIP(4) + dstIP(4) + formatVer(1) + flags(1) + [token(16)] + data(N)
// Old format (no formatVer): srcIP(4) + dstIP(4) + IPv4data(N)
const DataFormatVersion byte = 0x01

// DataPayload carries a relayed IP packet between client and server.
// Wire format (v1.8+): srcIP(4) + dstIP(4) + formatVer(1) + flags(1) + [token(16)] + data(N)
// Wire format (legacy): srcIP(4) + dstIP(4) + IPv4data(N)
//
// The formatVer byte (0x01) explicitly distinguishes new format from legacy
// packets. Legacy packets have IPv4 data starting at byte 8 (version nibble
// 0x4x-0xFx), which never collides with formatVer=0x01.
type DataPayload struct {
	SrcIP     net.IP
	DstIP     net.IP
	FormatVer byte   // DataFormatVersion (0 = old format without version)
	Flags     byte   // DataFlagHasToken etc.
	Data      []byte
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
	dp.FormatVer = 0
	dp.Flags = 0
	dataPayloadPool.Put(dp)
}

func (d *DataPayload) Marshal() []byte {
	src := d.SrcIP.To4()
	dst := d.DstIP.To4()
	if src == nil || dst == nil {
		return nil
	}
	buf := make([]byte, 10+len(d.Data))
	copy(buf[0:4], src)
	copy(buf[4:8], dst)
	buf[8] = DataFormatVersion
	buf[9] = d.Flags
	copy(buf[10:], d.Data)
	return buf
}

// MarshalSize returns the encoded size of this DataPayload.
func (d *DataPayload) MarshalSize() int {
	return 10 + len(d.Data)
}

// MarshalTo writes the encoded payload into dst (zero-copy).
// Returns number of bytes written. Caller must ensure len(dst) >= MarshalSize().
func (d *DataPayload) MarshalTo(dst []byte) int {
	src := d.SrcIP.To4()
	dstIP := d.DstIP.To4()
	if src == nil || dstIP == nil || len(dst) < 10 {
		return 0
	}
	copy(dst[0:4], src)
	copy(dst[4:8], dstIP)
	dst[8] = DataFormatVersion
	dst[9] = d.Flags
	return 10 + copy(dst[10:], d.Data)
}

func UnmarshalData(data []byte) (*DataPayload, error) {
	if len(data) < 8 {
		return nil, ErrPacketTooShort
	}
	dp := &DataPayload{
		SrcIP: net.IP(append([]byte(nil), data[0:4]...)),
		DstIP: net.IP(append([]byte(nil), data[4:8]...)),
	}
	if len(data) > 8 && data[8] == DataFormatVersion {
		// New format: formatVer(1) + flags(1) + [token(16)] + data(N)
		dp.FormatVer = data[8]
		dp.Flags = data[9]
		offset := 10
		if dp.Flags&DataFlagHasToken != 0 {
			offset += 16
		}
		if offset > len(data) {
			return nil, ErrPacketTooShort
		}
		dataLen := len(data) - offset
		pktData := make([]byte, dataLen)
		copy(pktData, data[offset:])
		dp.Data = pktData
	} else {
		// Old format (legacy): raw IPv4 data directly after dstIP
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
	if len(data) > 8 && data[8] == DataFormatVersion {
		// New format: formatVer(1) + flags(1) + [token(16)] + data(N)
		dp.FormatVer = data[8]
		dp.Flags = data[9]
		offset := 10
		if dp.Flags&DataFlagHasToken != 0 {
			offset += 16
		}
		if offset > len(data) {
			PutDataPayload(dp)
			return nil, ErrPacketTooShort
		}
		rawData := data[offset:]
		if cap(dp.Data) < len(rawData) {
			dp.Data = make([]byte, len(rawData))
		} else {
			dp.Data = dp.Data[:len(rawData)]
		}
		copy(dp.Data, rawData)
	} else {
		dp.FormatVer = 0
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


