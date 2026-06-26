package protocol

import (
	"encoding/binary"
	"net"
)

// ── Register ───────────────────────────────────────────────────

// RegisterPayload is sent by the client to join a room.
type RegisterPayload struct {
	RoomID   string
	Username string
	Version  uint16 // client protocol version (0 = old client without version)
}

func (r *RegisterPayload) Marshal() []byte {
	roomBytes := []byte(r.RoomID)
	userBytes := []byte(r.Username)
	buf := make([]byte, 2+len(roomBytes)+2+len(userBytes)+2)
	off := 0
	binary.LittleEndian.PutUint16(buf[off:], uint16(len(roomBytes)))
	off += 2
	copy(buf[off:], roomBytes)
	off += len(roomBytes)
	binary.LittleEndian.PutUint16(buf[off:], uint16(len(userBytes)))
	off += 2
	copy(buf[off:], userBytes)
	off += len(userBytes)
	binary.LittleEndian.PutUint16(buf[off:], r.Version)
	return buf
}

func UnmarshalRegister(data []byte) (*RegisterPayload, error) {
	if len(data) < 4 {
		return nil, ErrPacketTooShort
	}
	off := 0
	roomLen := int(binary.LittleEndian.Uint16(data[off:]))
	off += 2
	if len(data) < off+roomLen+2 {
		return nil, ErrPacketTooShort
	}
	roomID := string(data[off : off+roomLen])
	off += roomLen
	userLen := int(binary.LittleEndian.Uint16(data[off:]))
	off += 2
	if len(data) < off+userLen {
		return nil, ErrPacketTooShort
	}
	username := string(data[off : off+userLen])
	off += userLen
	result := &RegisterPayload{RoomID: roomID, Username: username}
	// Version is appended at the end (backward compatible: old clients don't send it)
	if len(data) >= off+2 {
		result.Version = binary.LittleEndian.Uint16(data[off:])
	}
	return result, nil
}

// ── Assign IP ──────────────────────────────────────────────────

// AssignIPPayload is sent by the server to assign a virtual IP to a client.
type AssignIPPayload struct {
	VirtualIP  net.IP
	SubnetMask net.IPMask
	ServerIP   net.IP
	Version    uint16 // server protocol version (0 = old server without version)
}

func (a *AssignIPPayload) Marshal() []byte {
	vip := a.VirtualIP.To4()
	mask := net.IP(a.SubnetMask).To4()
	srv := a.ServerIP.To4()
	if vip == nil || mask == nil || srv == nil {
		return nil
	}
	buf := make([]byte, 14)
	copy(buf[0:4], vip)
	copy(buf[4:8], mask)
	copy(buf[8:12], srv)
	binary.LittleEndian.PutUint16(buf[12:14], a.Version)
	return buf
}

func UnmarshalAssignIP(data []byte) (*AssignIPPayload, error) {
	if len(data) < 12 {
		return nil, ErrPacketTooShort
	}
	result := &AssignIPPayload{
		VirtualIP:  net.IP(append([]byte(nil), data[0:4]...)),
		SubnetMask: net.IPMask(append([]byte(nil), data[4:8]...)),
		ServerIP:   net.IP(append([]byte(nil), data[8:12]...)),
	}
	// Version is appended at the end (backward compatible: old servers don't send it)
	if len(data) >= 14 {
		result.Version = binary.LittleEndian.Uint16(data[12:14])
	}
	return result, nil
}

// ── Kick ───────────────────────────────────────────────────────

// KickCode identifies the reason for a kick using a numeric code,
// enabling reliable client-side matching independent of localized strings.
type KickCode byte

const (
	KickCodeNone            KickCode = 0 // unknown / generic (recoverable)
	KickCodeWrongPassword   KickCode = 1 // wrong password (fatal, stop reconnect)
	KickCodeVersionMismatch KickCode = 2 // version incompatible (fatal, stop reconnect)
	KickCodeShutdown        KickCode = 3 // server shutting down
)

// KickPayload is sent by the server to reject or disconnect a client.
type KickPayload struct {
	Reason string
	Code   KickCode
}

// Marshal encodes the kick payload. Wire format:
//
//	[2 bytes: reasonLen][reasonBytes][1 byte: code]
//
// The trailing code byte is backward-compatible: old clients that only read
// reasonLen+reasonBytes will ignore the extra byte. Old servers that don't
// send the code byte will have clients default to KickCodeNone (0).
func (k *KickPayload) Marshal() []byte {
	reasonBytes := []byte(k.Reason)
	buf := make([]byte, 2+len(reasonBytes)+1)
	binary.LittleEndian.PutUint16(buf, uint16(len(reasonBytes)))
	copy(buf[2:], reasonBytes)
	buf[2+len(reasonBytes)] = byte(k.Code)
	return buf
}

// UnmarshalKick decodes a kick payload. The code byte is optional for
// backward compatibility with older servers that don't send it.
func UnmarshalKick(data []byte) (*KickPayload, error) {
	if len(data) < 2 {
		return nil, ErrPacketTooShort
	}
	reasonLen := int(binary.LittleEndian.Uint16(data))
	if len(data) < 2+reasonLen {
		return nil, ErrPacketTooShort
	}
	k := &KickPayload{Reason: string(data[2 : 2+reasonLen])}
	if len(data) >= 2+reasonLen+1 {
		k.Code = KickCode(data[2+reasonLen])
	}
	return k, nil
}

// ── Ping/Pong ─────────────────────────────────────────────────

// PingPayload carries a timestamp for RTT measurement.
// Server sends TypePing, client echoes it back as TypePong.
type PingPayload struct {
	Timestamp int64 // unix timestamp in nanoseconds
}

func (p *PingPayload) Marshal() []byte {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, uint64(p.Timestamp))
	return buf
}

func UnmarshalPing(data []byte) (*PingPayload, error) {
	if len(data) < 8 {
		return nil, ErrPacketTooShort
	}
	return &PingPayload{Timestamp: int64(binary.LittleEndian.Uint64(data))}, nil
}
