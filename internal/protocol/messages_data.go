package protocol

import (
	"net"
	"sync"
)

// ── Data (relay) ───────────────────────────────────────────────

// DataFlagHasToken is set in Flags when a 16-byte session token follows the flags byte.
// Only used when both client and server are v1.7+.
const DataFlagHasToken byte = 0x02

// DataFormatVersion is the explicit format version byte written after dstIP
// in the DataPayload wire format. Replaces the old isNewFormat heuristic.
// 0x01 = current format: srcIP(4) + dstIP(4) + formatVer(1) + flags(1) + [token(16)] + data(N)
// Old format (no formatVer): srcIP(4) + dstIP(4) + IPv4data(N)
const DataFormatVersion byte = 0x01

// DataHeaderLen is the fixed header length for the new wire format:
// srcIP(4) + dstIP(4) + formatVer(1) + flags(1).
const DataHeaderLen = 10

// DataTokenLen is the length of the session token.
const DataTokenLen = 16

// ParseDataHeader parses the data payload header for token validation.
// Returns the flags byte, the offset where a 16-byte session token starts
// (if present), and whether the new format (v1.8+) was detected.
// If isNew is false, the payload uses the old format (flags byte at [8],
// token at [9], no format version byte).
// Callers must check flags&DataFlagHasToken before reading the token.
func ParseDataHeader(data []byte) (flags byte, tokenOff int, isNew bool) {
	if len(data) <= 8 {
		return 0, 0, false
	}
	if data[8] == DataFormatVersion {
		if len(data) > 9 {
			return data[9], DataHeaderLen, true
		}
		return 0, DataHeaderLen, true
	}
	return data[8], 9, false
}

// dataOffset returns the offset where the raw IP payload data begins.
// For new format this accounts for the optional token; for legacy (no format
// version) data starts right after dstIP.
func dataOffset(data []byte) int {
	if len(data) > 8 && data[8] == DataFormatVersion {
		off := DataHeaderLen
		if len(data) > 9 && data[9]&DataFlagHasToken != 0 {
			off += DataTokenLen
		}
		return off
	}
	return 8
}

// DataPayload carries a relayed IP packet between client and server.
// Wire format (v1.8+): srcIP(4) + dstIP(4) + formatVer(1) + flags(1) + [token(16)] + data(N)
// Wire format (legacy): srcIP(4) + dstIP(4) + IPv4data(N)
type DataPayload struct {
	SrcIP     net.IP
	DstIP     net.IP
	FormatVer byte   // DataFormatVersion (0 = old format without version)
	Flags     byte   // DataFlagHasToken etc.
	Token     [DataTokenLen]byte // session token (used when Flags&DataFlagHasToken != 0)
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
	dp.Token = [DataTokenLen]byte{}
	dataPayloadPool.Put(dp)
}

func (d *DataPayload) Marshal() []byte {
	src := d.SrcIP.To4()
	dst := d.DstIP.To4()
	if src == nil || dst == nil {
		return nil
	}
	size := DataHeaderLen + len(d.Data)
	if d.Flags&DataFlagHasToken != 0 {
		size += DataTokenLen
	}
	buf := make([]byte, size)
	copy(buf[0:4], src)
	copy(buf[4:8], dst)
	buf[8] = DataFormatVersion
	buf[9] = d.Flags
	off := DataHeaderLen
	if d.Flags&DataFlagHasToken != 0 {
		copy(buf[off:], d.Token[:])
		off += DataTokenLen
	}
	copy(buf[off:], d.Data)
	return buf
}

// MarshalSize returns the encoded size of this DataPayload.
func (d *DataPayload) MarshalSize() int {
	size := DataHeaderLen + len(d.Data)
	if d.Flags&DataFlagHasToken != 0 {
		size += DataTokenLen
	}
	return size
}

// MarshalTo writes the encoded payload into dst (zero-copy).
// Returns number of bytes written. Caller must ensure len(dst) >= MarshalSize().
func (d *DataPayload) MarshalTo(dst []byte) int {
	src := d.SrcIP.To4()
	dstIP := d.DstIP.To4()
	if src == nil || dstIP == nil || len(dst) < DataHeaderLen {
		return 0
	}
	copy(dst[0:4], src)
	copy(dst[4:8], dstIP)
	dst[8] = DataFormatVersion
	dst[9] = d.Flags
	off := DataHeaderLen
	if d.Flags&DataFlagHasToken != 0 {
		copy(dst[off:], d.Token[:])
		off += DataTokenLen
	}
	return off + copy(dst[off:], d.Data)
}

func UnmarshalData(data []byte) (*DataPayload, error) {
	if len(data) < 8 {
		return nil, ErrPacketTooShort
	}
	dp := &DataPayload{
		SrcIP: net.IP(append([]byte(nil), data[0:4]...)),
		DstIP: net.IP(append([]byte(nil), data[4:8]...)),
	}
	off := dataOffset(data)
	if off > len(data) {
		return nil, ErrPacketTooShort
	}
	if off != 8 {
		dp.Flags = data[9]
		dp.FormatVer = data[8]
	}
	if dp.Flags&DataFlagHasToken != 0 && len(data) >= DataHeaderLen+DataTokenLen {
		copy(dp.Token[:], data[DataHeaderLen:DataHeaderLen+DataTokenLen])
	}
	dataLen := len(data) - off
	pktData := make([]byte, dataLen)
	copy(pktData, data[off:])
	dp.Data = pktData
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

	off := dataOffset(data)
	if off > len(data) {
		PutDataPayload(dp)
		return nil, ErrPacketTooShort
	}
	if off != 8 {
		dp.Flags = data[9]
		dp.FormatVer = data[8]
	} else {
		dp.FormatVer = 0
		dp.Flags = 0
	}
	if dp.Flags&DataFlagHasToken != 0 && len(data) >= DataHeaderLen+DataTokenLen {
		copy(dp.Token[:], data[DataHeaderLen:DataHeaderLen+DataTokenLen])
	}
	rawData := data[off:]
	if cap(dp.Data) < len(rawData) {
		dp.Data = make([]byte, len(rawData))
	} else {
		dp.Data = dp.Data[:len(rawData)]
	}
	copy(dp.Data, rawData)
	return dp, nil
}
