package protocol

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


