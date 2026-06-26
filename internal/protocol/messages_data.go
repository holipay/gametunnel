package protocol

import (
	"net"
	"sync"
)

// ── Data (relay) ───────────────────────────────────────────────

// DataPayload carries a relayed IP packet between client and server.
type DataPayload struct {
	SrcIP net.IP
	DstIP net.IP
	Data  []byte
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
	buf := make([]byte, 8+len(d.Data))
	copy(buf[0:4], src)
	copy(buf[4:8], dst)
	copy(buf[8:], d.Data)
	return buf
}

// MarshalSize returns the encoded size of this DataPayload.
func (d *DataPayload) MarshalSize() int {
	return 8 + len(d.Data)
}

// MarshalTo writes the encoded payload into dst (zero-copy).
// Returns number of bytes written. Caller must ensure len(dst) >= MarshalSize().
func (d *DataPayload) MarshalTo(dst []byte) int {
	src := d.SrcIP.To4()
	dstIP := d.DstIP.To4()
	if src == nil || dstIP == nil || len(dst) < 8 {
		return 0
	}
	copy(dst[0:4], src)
	copy(dst[4:8], dstIP)
	return 8 + copy(dst[8:], d.Data)
}

func UnmarshalData(data []byte) (*DataPayload, error) {
	if len(data) < 8 {
		return nil, ErrPacketTooShort
	}
	pktData := make([]byte, len(data)-8)
	copy(pktData, data[8:])
	return &DataPayload{
		SrcIP: net.IP(append([]byte(nil), data[0:4]...)),
		DstIP: net.IP(append([]byte(nil), data[4:8]...)),
		Data:  pktData,
	}, nil
}

// UnmarshalDataPooled is like UnmarshalData but reuses a pooled DataPayload.
// The returned object MUST be released with PutDataPayload after use.
// Callers MUST NOT retain references to dp.Data after returning the payload
// to the pool (dp.Data may point to a shared buffer).
func UnmarshalDataPooled(data []byte) (*DataPayload, error) {
	if len(data) < 8 {
		return nil, ErrPacketTooShort
	}
	dp := dataPayloadPool.Get().(*DataPayload)
	dp.SrcIP = append(dp.SrcIP[:0], data[0:4]...)
	dp.DstIP = append(dp.DstIP[:0], data[4:8]...)
	dp.Data = append(dp.Data[:0], data[8:]...)
	return dp, nil
}
