// Package protocol defines the wire protocol between GameTunnel client and server.
//
// Wire format (v1):
//
//	[1 byte: version] [1 byte: type] [payload...] [4 bytes: CRC32]
//
// All multi-byte integers are little-endian.
//
// Authentication: HMAC challenge-response (room password → HKDF-SHA256 key)
// Integrity: CRC32 on every packet
package protocol

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"

	"golang.org/x/crypto/hkdf"
)

// Protocol version. Bump on breaking wire-format changes.
const ProtocolVersion byte = 1

// HeaderLen is the fixed header size: version(1) + type(1).
const HeaderLen = 2

// ChecksumLen is the CRC32 checksum size appended to every packet.
const ChecksumLen = 4

// ── Message Types ──────────────────────────────────────────────

const (
	TypeRegister       byte = 0x01 // client → server: join room
	TypeAssignIP       byte = 0x02 // server → client: virtual IP assigned
	TypePeerInfo       byte = 0x03 // server → client: peer endpoint info
	TypePeerRequest    byte = 0x04 // client → server: request peer list
	TypeHolePunch      byte = 0x05 // client ↔ client: NAT hole punch
	TypeData           byte = 0x06 // client ↔ server: relayed payload
	TypeKeepAlive      byte = 0x07 // client → server: keep connection alive
	TypeAuthChallenge  byte = 0x08 // server → client: auth challenge (nonce)
	TypeAuthResponse   byte = 0x09 // client → server: auth HMAC response
	TypeKick           byte = 0x0A // server → client: kicked / error
	TypeDisconnect     byte = 0x0B // client → server: graceful disconnect
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
// Does NOT include checksum — call AppendChecksum on the result.
func Encode(typ byte, payload []byte) []byte {
	buf := make([]byte, HeaderLen+len(payload))
	buf[0] = ProtocolVersion
	buf[1] = typ
	copy(buf[HeaderLen:], payload)
	return buf
}

// AppendChecksum appends a CRC32 checksum to a packet (mutates in-place, returns slice).
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
// Does NOT verify checksum — call VerifyChecksum first.
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

// ── Auth: Key Derivation (HKDF-SHA256) ─────────────────────────

// DeriveKey derives a 32-byte HMAC key from the room password using HKDF-SHA256.
// Room ID is used as "info" context to bind the key to a specific room.
// Returns nil if password is empty.
func DeriveKey(password, roomID string) []byte {
	if password == "" {
		return nil
	}
	hkdfReader := hkdf.New(sha256.New, []byte(password), nil, []byte("GameTunnel:"+roomID))
	key := make([]byte, 32)
	if _, err := io.ReadFull(hkdfReader, key); err != nil {
		return nil
	}
	return key
}

// ── Auth: Challenge-Response ───────────────────────────────────

// GenerateChallenge creates a 16-byte random nonce for authentication.
func GenerateChallenge() ([]byte, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate challenge: %w", err)
	}
	return nonce, nil
}

// ComputeAuthHMAC computes the HMAC-SHA256 over the challenge+context.
// Binds the response to: challenge nonce, room ID, username, and client address.
func ComputeAuthHMAC(key []byte, challenge []byte, roomID, username string, remoteAddr *net.UDPAddr) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(challenge)
	mac.Write([]byte(roomID))
	mac.Write([]byte(username))
	if remoteAddr != nil {
		mac.Write([]byte(remoteAddr.String()))
	}
	return mac.Sum(nil)
}

// VerifyAuthHMAC verifies the client's auth HMAC. Returns true if valid.
func VerifyAuthHMAC(key, clientHMAC []byte, challenge []byte, roomID, username string, remoteAddr *net.UDPAddr) bool {
	expected := ComputeAuthHMAC(key, challenge, roomID, username, remoteAddr)
	return hmac.Equal(clientHMAC, expected)
}

// ── Auth Challenge Payload (server → client) ───────────────────

// AuthChallengePayload is sent by the server to initiate authentication.
type AuthChallengePayload struct {
	Challenge []byte // 16-byte random nonce
}

func (a *AuthChallengePayload) Marshal() []byte {
	buf := make([]byte, 2+len(a.Challenge))
	binary.LittleEndian.PutUint16(buf, uint16(len(a.Challenge)))
	copy(buf[2:], a.Challenge)
	return buf
}

func UnmarshalAuthChallenge(data []byte) (*AuthChallengePayload, error) {
	if len(data) < 2 {
		return nil, ErrPacketTooShort
	}
	clen := int(binary.LittleEndian.Uint16(data[0:]))
	if len(data) < 2+clen {
		return nil, ErrPacketTooShort
	}
	challenge := make([]byte, clen)
	copy(challenge, data[2:2+clen])
	return &AuthChallengePayload{Challenge: challenge}, nil
}

// ── Auth Response Payload (client → server) ────────────────────

// AuthResponsePayload is sent by the client to prove knowledge of the room password.
type AuthResponsePayload struct {
	RoomID   string
	Username string
	HMAC     []byte // 32-byte HMAC-SHA256
}

func (a *AuthResponsePayload) Marshal() []byte {
	roomBytes := []byte(a.RoomID)
	userBytes := []byte(a.Username)
	buf := make([]byte, 2+len(roomBytes)+2+len(userBytes)+2+len(a.HMAC))
	off := 0
	binary.LittleEndian.PutUint16(buf[off:], uint16(len(roomBytes)))
	off += 2
	copy(buf[off:], roomBytes)
	off += len(roomBytes)
	binary.LittleEndian.PutUint16(buf[off:], uint16(len(userBytes)))
	off += 2
	copy(buf[off:], userBytes)
	off += len(userBytes)
	binary.LittleEndian.PutUint16(buf[off:], uint16(len(a.HMAC)))
	off += 2
	copy(buf[off:], a.HMAC)
	return buf
}

func UnmarshalAuthResponse(data []byte) (*AuthResponsePayload, error) {
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
	if len(data) < off+userLen+2 {
		return nil, ErrPacketTooShort
	}
	username := string(data[off : off+userLen])
	off += userLen
	hmacLen := int(binary.LittleEndian.Uint16(data[off:]))
	off += 2
	if len(data) < off+hmacLen {
		return nil, ErrPacketTooShort
	}
	hmacVal := make([]byte, hmacLen)
	copy(hmacVal, data[off:off+hmacLen])
	return &AuthResponsePayload{RoomID: roomID, Username: username, HMAC: hmacVal}, nil
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
	// Pre-calculate total size to avoid repeated append growth.
	total := 2 // peer count
	for _, peer := range p.Peers {
		total += 4 + 2 + len(peer.PublicAddr.String()) + 2 + len(peer.Username)
	}
	buf := make([]byte, 0, total)
	buf = append(buf, byte(len(p.Peers)), byte(len(p.Peers)>>8))
	for _, peer := range p.Peers {
		vip := peer.VirtualIP.To4()
		buf = append(buf, vip...)
		addrStr := ""
		if peer.PublicAddr != nil {
			addrStr = peer.PublicAddr.String()
		}
		addrBytes := []byte(addrStr)
		buf = append(buf, byte(len(addrBytes)), byte(len(addrBytes)>>8))
		buf = append(buf, addrBytes...)
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
	pktData := make([]byte, len(data)-8)
	copy(pktData, data[8:])
	return &DataPayload{
		SrcIP: net.IP(append([]byte(nil), data[0:4]...)),
		DstIP: net.IP(append([]byte(nil), data[4:8]...)),
		Data:  pktData,
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
