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
// v1.3 = 0x0103 = 259, v1.4 = 0x0104 = 260, v1.5 = 0x0105 = 261, v1.6 = 0x0106 = 262, v1.7 = 0x0107 = 263, v1.8 = 0x0108 = 264, v1.9 = 0x0109 = 265, v1.10 = 0x010A = 266, v1.11 = 0x010B = 267, v1.12 = 0x010C = 268
const AppVersion uint16 = 0x010C

// MinTokenVersion is the minimum server version that supports session tokens.
const MinTokenVersion uint16 = 0x0107

// MinRelayNoCRCVersion is the minimum server version that does NOT append
// a redundant CRC32 to relayed TypeData packets for encrypted rooms.
// AEAD (ChaCha20-Poly1305) already provides integrity, making the CRC
// unnecessary. Older servers still append it for backward compatibility.
// The client uses this constant to decide whether to strip the trailing CRC.
const MinRelayNoCRCVersion uint16 = 0x0108

// Client feature versions — minimum client version that supports a feature.
// Used for status page display and server-side client capability detection.
// Values mirror the server constants today but the separation makes the
// client contract explicit and prevents future drift if features diverge.
const (
	MinClientTokenVersion  uint16 = 0x0107 // client sends session token in relay packets
	MinClientNoCRCVersion  uint16 = 0x0108 // client strips trailing CRC from encrypted relay
)

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

	// P2P enhancement: NAT type probing
	TypeNATProbe    byte = 0x0E // client → server: NAT type probe request
	TypeNATResponse byte = 0x0F // server → client: NAT type probe response

	// Connection migration
	TypeRebind    byte = 0x10 // client → server: request address migration (WiFi↔4G)
	TypeRebindAck byte = 0x11 // server → client: migration confirmed

	// Forward secrecy: X25519 ECDH key exchange
	TypeECDHExchange byte = 0x12 // server → client: server's ephemeral public key
	TypeECDHConfirm  byte = 0x13 // client → server: client's ephemeral public key + HMAC
)

// ── Common Errors ──────────────────────────────────────────────

var (
	ErrPacketTooShort     = errors.New("packet too short")
	ErrUnsupportedVersion = errors.New("unsupported protocol version")
	ErrChecksumMismatch   = errors.New("CRC32 checksum mismatch")
	ErrTooManyPeers       = errors.New("too many peers in PeerInfo")
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
	return binary.LittleEndian.AppendUint32(packet, crc)
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
// The returned Payload is a copy of the input data to prevent aliasing.
func Decode(data []byte) (*Message, error) {
	if len(data) < HeaderLen {
		return nil, ErrPacketTooShort
	}
	if data[0] != ProtocolVersion {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrUnsupportedVersion, data[0], ProtocolVersion)
	}
	payload := make([]byte, len(data)-HeaderLen)
	copy(payload, data[HeaderLen:])
	return &Message{
		Type:    data[1],
		Payload: payload,
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

// DecodeLenient tries DecodeChecked first. If allowCRCFallback is true
// and the CRC32 check fails, it falls back to Decode (skipping checksum
// verification). This supports encrypted packets that omit CRC32 since
// ChaCha20-Poly1305 AEAD already provides integrity. Old packets with
// CRC32 still work transparently.
//
// For unencrypted rooms, pass allowCRCFallback=false to reject corrupted packets.
func DecodeLenient(data []byte, allowCRCFallback bool) (*Message, error) {
	msg, err := DecodeChecked(data)
	if err == nil {
		return msg, nil
	}
	if allowCRCFallback && errors.Is(err, ErrChecksumMismatch) {
		return Decode(data)
	}
	return nil, err
}

// DecodeSkipCRC decodes a packet without CRC32 verification.
// Use only when AEAD encryption provides integrity (roomPass != "").
func DecodeSkipCRC(data []byte) (*Message, error) {
	return Decode(data)
}

// EncodeChecked is a convenience: Encode + AppendChecksum.
// Combines into a single allocation to reduce GC pressure on the hot path.
func EncodeChecked(typ byte, payload []byte) []byte {
	buf := make([]byte, 0, HeaderLen+len(payload)+ChecksumLen)
	buf = append(buf, ProtocolVersion, typ)
	buf = append(buf, payload...)
	crc := crc32.ChecksumIEEE(buf)
	return binary.LittleEndian.AppendUint32(buf, crc)
}

// AppendEncodeChecked encodes a packet into dst (appending), avoiding allocation.
// Returns the extended slice. Caller must ensure dst has enough capacity.
func AppendEncodeChecked(dst []byte, typ byte, payload []byte) []byte {
	start := len(dst)
	// Header
	dst = append(dst, ProtocolVersion, typ)
	// Payload
	dst = append(dst, payload...)
	// CRC32 over header+payload only (not pre-existing dst content)
	crc := crc32.ChecksumIEEE(dst[start:])
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
//   - The ECDH flag (0x8000) is stripped before comparison — it is not a version component.
func IsCompatible(clientVer, serverVer uint16) bool {
	// Old clients/servers that don't send version are always allowed
	if clientVer == 0 || serverVer == 0 {
		return true
	}
	clientVer &^= versionECDHFlag
	serverVer &^= versionECDHFlag
	if VersionMajor(clientVer) != VersionMajor(serverVer) {
		return false
	}
	return VersionMinor(clientVer) <= VersionMinor(serverVer)
}
