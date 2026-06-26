package protocol

import "encoding/binary"

// ── Auth Challenge Payload (server → client) ───────────────────

// AuthChallengePayload is sent by the server to initiate authentication.
type AuthChallengePayload struct {
	Challenge  []byte // 16-byte random nonce
	ClientAddr string // client's public address as seen by server
}

func (a *AuthChallengePayload) Marshal() []byte {
	addrBytes := []byte(a.ClientAddr)
	buf := make([]byte, 2+len(a.Challenge)+2+len(addrBytes))
	binary.LittleEndian.PutUint16(buf, uint16(len(a.Challenge)))
	copy(buf[2:], a.Challenge)
	off := 2 + len(a.Challenge)
	binary.LittleEndian.PutUint16(buf[off:], uint16(len(addrBytes)))
	copy(buf[off+2:], addrBytes)
	return buf
}

func UnmarshalAuthChallenge(data []byte) (*AuthChallengePayload, error) {
	if len(data) < 2 {
		return nil, ErrPacketTooShort
	}
	clen := int(binary.LittleEndian.Uint16(data[0:]))
	if len(data) < 2+clen+2 {
		return nil, ErrPacketTooShort
	}
	challenge := make([]byte, clen)
	copy(challenge, data[2:2+clen])
	off := 2 + clen
	addrLen := int(binary.LittleEndian.Uint16(data[off:]))
	if len(data) < off+2+addrLen {
		return nil, ErrPacketTooShort
	}
	clientAddr := string(data[off+2 : off+2+addrLen])
	return &AuthChallengePayload{Challenge: challenge, ClientAddr: clientAddr}, nil
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
