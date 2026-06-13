// Package protocol defines the wire protocol between GameTunnel client and server.
//
// Wire format (v1):
//
//	[1 byte: version] [1 byte: type] [payload...] [4 bytes: CRC32]
//
// All multi-byte integers are little-endian.
package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// Protocol version. Bump on breaking wire-format changes.
const ProtocolVersion byte = 1

// AppVersion is the application version encoded as (major << 8 | minor).
// Used for client-server compatibility negotiation during handshake.
// v1.2 = 0x0102 = 258
const AppVersion uint16 = 0x0102

// HeaderLen is the fixed header size: version(1) + type(1).
const HeaderLen = 2

// ChecksumLen is the CRC32 checksum size appended to every packet.
const ChecksumLen = 4

// MaxPacketLen is the largest possible encoded packet (header + MTU + checksum).
const MaxPacketLen = HeaderLen + 1500 + ChecksumLen

// ── Message Types ──────────────────────────────────────────────

const (
	TypeRegister      byte = 0x01 // client → server: join room
	TypeAssignIP      byte = 0x02 // server → client: virtual IP assigned
	TypePeerInfo      byte = 0x03 // server → client: peer endpoint info
	TypePeerRequest   byte = 0x04 // client → server: request peer list
	TypeHolePunch     byte = 0x05 // client ↔ client: NAT hole punch
	TypeData          byte = 0x06 // client ↔ server: relayed payload
	TypeKeepAlive     byte = 0x07 // client → server: keep connection alive
	TypeAuthChallenge byte = 0x08 // server → client: auth challenge (nonce)
	TypeAuthResponse  byte = 0x09 // client → server: auth HMAC response
	TypeKick          byte = 0x0A // server → client: kicked / error
	TypeDisconnect    byte = 0x0B // client → server: graceful disconnect
	TypePing          byte = 0x0C // server → client: latency ping
	TypePong          byte = 0x0D // client → server: latency pong (echo)
)

// ── Common Errors ──────────────────────────────────────────────

var (
	ErrPacketTooShort     = errors.New("packet too short")
	ErrUnsupportedVersion = errors.New("unsupported protocol version")
	ErrChecksumMismatch   = errors.New("CRC32 checksum mismatch")
)

// ── Base Message ───────────────────────────────────────────────

// Message is a decoded protocol message.
type Message struct {
	Type    byte
	Payload []byte
}

// Encode prepends version + type bytes and returns raw bytes ready to send.
func Encode(typ byte, payload []byte) []byte {
	buf := make([]byte, HeaderLen+len(payload))
	buf[0] = ProtocolVersion
	buf[1] = typ
	copy(buf[HeaderLen:], payload)
	return buf
}

// AppendChecksum appends a CRC32 checksum to a packet.
func AppendChecksum(packet []byte) []byte {
	crc := crc32.ChecksumIEEE(packet)
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, crc)
	return append(packet, b...)
}

// VerifyChecksum validates the CRC32 at the end of a packet.
// Returns the packet without the checksum tail on success.
func VerifyChecksum(data []byte) ([]byte, error) {
	if len(data) < HeaderLen+ChecksumLen {
		return nil, ErrPacketTooShort
	}
	body := data[:len(data)-ChecksumLen]
	checksum := binary.LittleEndian.Uint32(data[len(data)-ChecksumLen:])
	if crc32.ChecksumIEEE(body) != checksum {
		return nil, ErrChecksumMismatch
	}
	return body, nil
}

// Decode extracts the version, message type and payload from a raw packet.
func Decode(data []byte) (*Message, error) {
	if len(data) < HeaderLen {
		return nil, ErrPacketTooShort
	}
	if data[0] != ProtocolVersion {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrUnsupportedVersion, data[0], ProtocolVersion)
	}
	return &Message{
		Type:    data[1],
		Payload: data[HeaderLen:],
	}, nil
}

// DecodeChecked is a convenience: VerifyChecksum + Decode.
func DecodeChecked(data []byte) (*Message, error) {
	body, err := VerifyChecksum(data)
	if err != nil {
		return nil, err
	}
	return Decode(body)
}

// EncodeChecked is a convenience: Encode + AppendChecksum.
func EncodeChecked(typ byte, payload []byte) []byte {
	return AppendChecksum(Encode(typ, payload))
}

// AppendEncodeChecked encodes a packet into dst (appending), avoiding allocation.
// Returns the extended slice. Caller must ensure dst has enough capacity.
func AppendEncodeChecked(dst []byte, typ byte, payload []byte) []byte {
	// Header
	dst = append(dst, ProtocolVersion, typ)
	// Payload
	dst = append(dst, payload...)
	// CRC32 over header+payload
	crc := crc32.ChecksumIEEE(dst)
	dst = append(dst,
		byte(crc),
		byte(crc>>8),
		byte(crc>>16),
		byte(crc>>24),
	)
	return dst
}

// ── Version Compatibility ─────────────────────────────────────

// VersionMajor returns the major version from an encoded version number.
func VersionMajor(v uint16) uint16 { return v >> 8 }

// VersionMinor returns the minor version from an encoded version number.
func VersionMinor(v uint16) uint16 { return v & 0xFF }

// IsCompatible checks if two application versions are compatible.
// Rules:
//   - Major version must match (breaking wire-format change)
//   - Client minor version must be ≤ server minor version (server supports older clients)
//   - Version 0 means "unknown" (old client/server without version field) — always compatible
func IsCompatible(clientVer, serverVer uint16) bool {
	// Old clients/servers that don't send version are always allowed
	if clientVer == 0 || serverVer == 0 {
		return true
	}
	if VersionMajor(clientVer) != VersionMajor(serverVer) {
		return false
	}
	return VersionMinor(clientVer) <= VersionMinor(serverVer)
}
