package protocol

import "errors"

// ErrWeakPublicKey indicates the received X25519 public key is a known weak point.
var ErrWeakPublicKey = errors.New("ecdh: weak public key (zero or identity)")

// isWeakX25519Key checks if a 32-byte key is a known weak X25519 point.
// Rejects the all-zero key and the identity element (scalar 1).
func isWeakX25519Key(key []byte) bool {
	if len(key) != 32 {
		return false
	}
	// Check all zeros
	allZero := true
	for _, b := range key {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return true
	}
	// Check identity element: little-endian encoding of 1
	if key[0] != 1 {
		return false
	}
	for _, b := range key[1:] {
		if b != 0 {
			return false
		}
	}
	return true
}

// ── ECDH Key Exchange ──────────────────────────────────────────

// ECDHExchangePayload is sent by the server after successful HMAC auth.
// Contains the server's ephemeral X25519 public key.
type ECDHExchangePayload struct {
	PublicKey [32]byte
}

func (e *ECDHExchangePayload) Marshal() []byte {
	buf := make([]byte, 32)
	copy(buf, e.PublicKey[:])
	return buf
}

func UnmarshalECDHExchange(data []byte) (*ECDHExchangePayload, error) {
	if len(data) < 32 {
		return nil, ErrPacketTooShort
	}
	p := &ECDHExchangePayload{}
	copy(p.PublicKey[:], data[:32])
	if isWeakX25519Key(p.PublicKey[:]) {
		return nil, ErrWeakPublicKey
	}
	return p, nil
}

// ECDHConfirmPayload is sent by the client after receiving the server's key.
// Contains the client's ephemeral X25519 public key + HMAC over both pubkeys
// (using the password-derived key) to prevent MITM.
type ECDHConfirmPayload struct {
	PublicKey [32]byte
	HMAC      [32]byte
}

func (e *ECDHConfirmPayload) Marshal() []byte {
	buf := make([]byte, 64)
	copy(buf[0:32], e.PublicKey[:])
	copy(buf[32:64], e.HMAC[:])
	return buf
}

func UnmarshalECDHConfirm(data []byte) (*ECDHConfirmPayload, error) {
	if len(data) < 64 {
		return nil, ErrPacketTooShort
	}
	p := &ECDHConfirmPayload{}
	copy(p.PublicKey[:], data[:32])
	copy(p.HMAC[:], data[32:64])
	if isWeakX25519Key(p.PublicKey[:]) {
		return nil, ErrWeakPublicKey
	}
	return p, nil
}

// ECDHConfirmFlag indicates that the server negotiated an ECDH session key.
// Sent in AssignIPPayload.Version high bits or as a separate flag byte.
// For now, the presence of a non-zero SessionToken in AssignIP implies
// ECDH was negotiated (both features ship together in v1.7).

// versionECDHMask is embedded in the AssignIP version field.
// If set, the client should derive the session key from ECDH shared secret.
const versionECDHFlag uint16 = 0x8000

// IsECDHNegotiated returns true if the version field indicates ECDH was used.
func IsECDHNegotiated(version uint16) bool {
	return version&versionECDHFlag != 0
}

// SetECDHFlag sets the ECDH flag in a version field.
func SetECDHFlag(version uint16) uint16 {
	return version | versionECDHFlag
}

// ClearECDHFlag clears the ECDH flag from a version field, returning the base version.
func ClearECDHFlag(version uint16) uint16 {
	return version & ^versionECDHFlag
}


