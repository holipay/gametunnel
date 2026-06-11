package protocol

import (
	"encoding/binary"
	"net"
	"strconv"
	"sync"
)

// ── Register ───────────────────────────────────────────────────

// RegisterPayload is sent by the client to join a room.
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

// AssignIPPayload is sent by the server to assign a virtual IP to a client.
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

// PeerInfoEntry describes a single peer in the peer list.
type PeerInfoEntry struct {
	VirtualIP  net.IP
	PublicAddr *net.UDPAddr
	Username   string
}

// PeerInfoPayload is sent by the server to inform clients about peers.
type PeerInfoPayload struct {
	Peers []PeerInfoEntry
}

// peerInfoPayloadPool reuses PeerInfoPayload objects to reduce GC pressure.
var peerInfoPayloadPool = sync.Pool{
	New: func() interface{} { return &PeerInfoPayload{} },
}

// GetPeerInfoPayload gets a PeerInfoPayload from the pool.
func GetPeerInfoPayload() *PeerInfoPayload {
	return peerInfoPayloadPool.Get().(*PeerInfoPayload)
}

// PutPeerInfoPayload returns a PeerInfoPayload to the pool.
// Callers MUST NOT use the object after calling this.
func PutPeerInfoPayload(p *PeerInfoPayload) {
	p.Peers = nil
	peerInfoPayloadPool.Put(p)
}

func (p *PeerInfoPayload) Marshal() []byte {
	// Pre-calculate total size to avoid multiple allocations
	total := 2 // peer count (2 bytes)
	for _, peer := range p.Peers {
		total += 4 // VirtualIP (4 bytes IPv4)
		total += 2 // addr length prefix
		if peer.PublicAddr != nil {
			total += len(peer.PublicAddr.String())
		}
		total += 2 // username length prefix
		total += len(peer.Username)
	}

	buf := make([]byte, 0, total)
	buf = append(buf, byte(len(p.Peers)), byte(len(p.Peers)>>8))
	for _, peer := range p.Peers {
		vip := peer.VirtualIP.To4()
		if len(vip) == 4 {
			buf = append(buf, vip...)
		} else {
			buf = append(buf, 0, 0, 0, 0) // fallback
		}
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
			if host, portStr, err := net.SplitHostPort(addrStr); err == nil {
				if port, err := strconv.Atoi(portStr); err == nil {
					if ip := net.ParseIP(host); ip != nil {
						pubAddr = &net.UDPAddr{IP: ip, Port: port}
					}
				}
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
	if src == nil || dstIP == nil {
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

// ── Kick ───────────────────────────────────────────────────────

// KickPayload is sent by the server to reject or disconnect a client.
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
