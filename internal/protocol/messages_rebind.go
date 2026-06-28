package protocol

import (
	"encoding/binary"
	"net"
)

// ── Rebind (client → server) ───────────────────────────────────

// RebindPayload is sent by the client to request address migration.
// When a client's network changes (WiFi↔4G, NAT rebinding), it sends
// this message from the new address to reclaim its session.
//
// The server verifies the HMAC (if room has a password) or falls back
// to virtual IP + recent lastSeen (if no password).
//
// Wire format:
//
//	[4B virtualIP] [2B hmacLen] [hmacLen B hmac]
//
// HMAC covers: virtualIP (bound to the session, prevents hijacking)
type RebindPayload struct {
	VirtualIP net.IP
	HMAC      []byte // HMAC-SHA256(key, virtualIP) — empty if no password
}

func (r *RebindPayload) Marshal() []byte {
	vip := r.VirtualIP.To4()
	if vip == nil {
		return nil
	}
	hmacLen := len(r.HMAC)
	buf := make([]byte, 4+2+hmacLen)
	copy(buf[0:4], vip)
	binary.LittleEndian.PutUint16(buf[4:6], uint16(hmacLen))
	copy(buf[6:], r.HMAC)
	return buf
}

func UnmarshalRebind(data []byte) (*RebindPayload, error) {
	if len(data) < 6 {
		return nil, ErrPacketTooShort
	}
	vip := net.IP(append([]byte(nil), data[0:4]...))
	hmacLen := int(binary.LittleEndian.Uint16(data[4:6]))
	if len(data) < 6+hmacLen {
		return nil, ErrPacketTooShort
	}
	r := &RebindPayload{VirtualIP: vip}
	if hmacLen > 0 {
		r.HMAC = make([]byte, hmacLen)
		copy(r.HMAC, data[6:6+hmacLen])
	}
	return r, nil
}

// ── RebindAck (server → client) ────────────────────────────────

// RebindAckPayload is sent by the server to confirm address migration.
// Wire format: [1B success] — 1 = OK, 0 = rejected
type RebindAckPayload struct {
	Success bool
}

func (a *RebindAckPayload) Marshal() []byte {
	if a.Success {
		return []byte{1}
	}
	return []byte{0}
}

func UnmarshalRebindAck(data []byte) (*RebindAckPayload, error) {
	if len(data) < 1 {
		return nil, ErrPacketTooShort
	}
	return &RebindAckPayload{Success: data[0] == 1}, nil
}
