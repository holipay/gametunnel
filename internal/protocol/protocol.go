// Package protocol defines the wire protocol between GameTunnel client and server.
//
// Wire format (v1):
//
//	[1 byte: version] [1 byte: type] [payload...]
//
// All multi-byte integers are little-endian.
package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
)

// Protocol version. Bump on breaking wire-format changes.
const ProtocolVersion byte = 1

// ── Message Types ──────────────────────────────────────────────

const (
	TypeRegister    byte = 0x01 // client → server: join room
	TypeAssignIP    byte = 0x02 // server → client: virtual IP assigned
	TypePeerInfo    byte = 0x03 // server → client: peer endpoint info
	TypePeerRequest byte = 0x04 // client → server: request peer list
	TypeHolePunch   byte = 0x05 // client ↔ client: NAT hole punch
	TypeData        byte = 0x06 // client ↔ server: relayed payload
	TypeKeepAlive   byte = 0x07 // client → server: keep connection alive
	TypeKick        byte = 0x09 // server → client: kicked / error
)

// ── Common Errors ──────────────────────────────────────────────

var (
	ErrPacketTooShort     = errors.New("packet too short")
	ErrUnsupportedVersion = errors.New("unsupported protocol version")
)

// ── Base Message ───────────────────────────────────────────────

// Message is a decoded protocol message.
type Message struct {
	Type    byte
	Payload []byte
}

// Encode prepends version + type bytes and returns raw bytes ready to send.
func Encode(typ byte, payload []byte) []byte {
	buf := make([]byte, 2+len(payload))
	buf[0] = ProtocolVersion
	buf[1] = typ
	copy(buf[2:], payload)
	return buf
}

// Decode extracts the version, message type and payload from a raw packet.
func Decode(data []byte) (*Message, error) {
	if len(data) < 2 {
		return nil, ErrPacketTooShort
	}
	if data[0] != ProtocolVersion {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrUnsupportedVersion, data[0], ProtocolVersion)
	}
	return &Message{
		Type:    data[1],
		Payload: data[2:],
	}, nil
}

// ── Register ───────────────────────────────────────────────────

// RegisterPayload is sent by client to join a room.
type RegisterPayload struct {
	RoomID   string
	Username string
}

func (r *RegisterPayload) Marshal() []byte {
	roomBytes := []byte(r.RoomID)
	userBytes := []byte(r.Username)
	buf := make([]byte, 2+len(roomBytes)+2+len(userBytes))
	off := 0
	binary.LittleEndian.PutUint16(buf[off:], uint16(len(roomBytes)))
	off += 2
	copy(buf[off:], roomBytes)
	off += len(roomBytes)
	binary.LittleEndian.PutUint16(buf[off:], uint16(len(userBytes)))
	off += 2
	copy(buf[off:], userBytes)
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
	return &RegisterPayload{RoomID: roomID, Username: username}, nil
}

// ── Assign IP ──────────────────────────────────────────────────

// AssignIPPayload is sent by server after successful registration.
type AssignIPPayload struct {
	VirtualIP  net.IP
	SubnetMask net.IPMask
	ServerIP   net.IP
}

func (a *AssignIPPayload) Marshal() []byte {
	buf := make([]byte, 12)
	copy(buf[0:4], a.VirtualIP.To4())
	copy(buf[4:8], net.IP(a.SubnetMask).To4())
	copy(buf[8:12], a.ServerIP.To4())
	return buf
}

func UnmarshalAssignIP(data []byte) (*AssignIPPayload, error) {
	if len(data) < 12 {
		return nil, ErrPacketTooShort
	}
	return &AssignIPPayload{
		VirtualIP:  net.IP(append([]byte(nil), data[0:4]...)),
		SubnetMask: net.IPMask(append([]byte(nil), data[4:8]...)),
		ServerIP:   net.IP(append([]byte(nil), data[8:12]...)),
	}, nil
}

// ── Peer Info ──────────────────────────────────────────────────

// PeerInfoEntry describes one peer.
type PeerInfoEntry struct {
	VirtualIP  net.IP
	PublicAddr *net.UDPAddr
	Username   string
}

// PeerInfoPayload carries info about all current peers.
type PeerInfoPayload struct {
	Peers []PeerInfoEntry
}

func (p *PeerInfoPayload) Marshal() []byte {
	var buf []byte
	// peer count (uint16)
	buf = append(buf, byte(len(p.Peers)), byte(len(p.Peers)>>8))
	for _, peer := range p.Peers {
		vip := peer.VirtualIP.To4()
		buf = append(buf, vip...)
		// public addr as string (uint16 length)
		addrStr := ""
		if peer.PublicAddr != nil {
			addrStr = peer.PublicAddr.String()
		}
		addrBytes := []byte(addrStr)
		buf = append(buf, byte(len(addrBytes)), byte(len(addrBytes)>>8))
		buf = append(buf, addrBytes...)
		// username (uint16 length)
		userBytes := []byte(peer.Username)
		buf = append(buf, byte(len(userBytes)), byte(len(userBytes)>>8))
		buf = append(buf, userBytes...)
	}
	return buf
}

func UnmarshalPeerInfo(data []byte) (*PeerInfoPayload, error) {
	if len(data) < 2 {
		return nil, ErrPacketTooShort
	}
	count := int(binary.LittleEndian.Uint16(data[0:2]))
	off := 2
	payload := &PeerInfoPayload{Peers: make([]PeerInfoEntry, 0, count)}
	for i := 0; i < count; i++ {
		if len(data) < off+4+2 {
			return nil, ErrPacketTooShort
		}
		vip := net.IP(append([]byte(nil), data[off:off+4]...))
		off += 4
		addrLen := int(binary.LittleEndian.Uint16(data[off:]))
		off += 2
		if len(data) < off+addrLen+2 {
			return nil, ErrPacketTooShort
		}
		addrStr := string(data[off : off+addrLen])
		off += addrLen
		var pubAddr *net.UDPAddr
		if addrStr != "" {
			a, err := net.ResolveUDPAddr("udp4", addrStr)
			if err == nil {
				pubAddr = a
			}
		}
		userLen := int(binary.LittleEndian.Uint16(data[off:]))
		off += 2
		if len(data) < off+userLen {
			return nil, ErrPacketTooShort
		}
		username := string(data[off : off+userLen])
		off += userLen
		payload.Peers = append(payload.Peers, PeerInfoEntry{
			VirtualIP:  vip,
			PublicAddr: pubAddr,
			Username:   username,
		})
	}
	return payload, nil
}

// ── Data (relay) ───────────────────────────────────────────────

// DataPayload wraps a packet to be relayed.
type DataPayload struct {
	SrcIP net.IP
	DstIP net.IP
	Data  []byte
}

func (d *DataPayload) Marshal() []byte {
	src := d.SrcIP.To4()
	dst := d.DstIP.To4()
	buf := make([]byte, 8+len(d.Data))
	copy(buf[0:4], src)
	copy(buf[4:8], dst)
	copy(buf[8:], d.Data)
	return buf
}

func UnmarshalData(data []byte) (*DataPayload, error) {
	if len(data) < 8 {
		return nil, ErrPacketTooShort
	}
	return &DataPayload{
		SrcIP: net.IP(append([]byte(nil), data[0:4]...)),
		DstIP: net.IP(append([]byte(nil), data[4:8]...)),
		Data:  data[8:],
	}, nil
}

// ── Kick ───────────────────────────────────────────────────────

type KickPayload struct {
	Reason string
}

func (k *KickPayload) Marshal() []byte {
	reasonBytes := []byte(k.Reason)
	buf := make([]byte, 2+len(reasonBytes))
	binary.LittleEndian.PutUint16(buf, uint16(len(reasonBytes)))
	copy(buf[2:], reasonBytes)
	return buf
}

func UnmarshalKick(data []byte) (*KickPayload, error) {
	if len(data) < 2 {
		return nil, ErrPacketTooShort
	}
	reasonLen := int(binary.LittleEndian.Uint16(data))
	if len(data) < 2+reasonLen {
		return nil, ErrPacketTooShort
	}
	return &KickPayload{Reason: string(data[2 : 2+reasonLen])}, nil
}
