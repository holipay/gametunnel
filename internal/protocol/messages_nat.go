package protocol

import (
	"encoding/binary"
	"net"
)

// ── NAT Probe (client → server) ────────────────────────────────

// NATProbePayload is sent by the client to request NAT type detection.
// The server responds with TypeNATResponse containing the client's
// observed external address and NAT type hints.
type NATProbePayload struct {
	ProbeID byte // probe sequence number (0, 1, 2...) for multi-probe detection
}

func (p *NATProbePayload) Marshal() []byte {
	return []byte{p.ProbeID}
}

func UnmarshalNATProbe(data []byte) (*NATProbePayload, error) {
	if len(data) < 1 {
		return nil, ErrPacketTooShort
	}
	return &NATProbePayload{ProbeID: data[0]}, nil
}

// ── NAT Response (server → client) ─────────────────────────────

// NATType classifies the client's NAT behavior.
type NATType byte

const (
	NATTypeUnknown          NATType = 0 // could not determine
	NATTypeFullCone         NATType = 1 // endpoint-independent mapping, easiest to punch
	NATTypeRestrictedCone   NATType = 2 // address-dependent mapping
	NATTypePortRestricted   NATType = 3 // address+port-dependent mapping
	NATTypeSymmetric        NATType = 4 // each destination gets a different mapping, hardest
	NATTypeNoNAT            NATType = 5 // public IP, no NAT
)

// NATResponsePayload is sent by the server in response to NATProbe.
//
// Wire format:
//
//	[1B probeID] [1B natType] [2B addrLen] [addrLen B observedAddr] [2B altAddrLen] [altAddrLen B altAddr]
//
// observedAddr: the client's external address as seen by the server (ip:port)
// altAddr: the client's external address as seen from a different server port (ip:port)
//   - If observedAddr and altAddr have the same IP and port → Full Cone or No NAT
//   - Same IP, different port → Port Restricted Cone or Symmetric
//   - Different IP → Symmetric (carrier-grade NAT)
type NATResponsePayload struct {
	ProbeID      byte
	NATType      NATType
	ObservedAddr *net.UDPAddr // client's external address from server's main port
	AltAddr      *net.UDPAddr // client's external address from server's alt port (may be nil)
}

func (r *NATResponsePayload) Marshal() []byte {
	observed := addrBytes(r.ObservedAddr)
	alt := addrBytes(r.AltAddr)
	buf := make([]byte, 1+1+2+len(observed)+2+len(alt))
	buf[0] = r.ProbeID
	buf[1] = byte(r.NATType)
	off := 2
	binary.LittleEndian.PutUint16(buf[off:], uint16(len(observed)))
	off += 2
	copy(buf[off:], observed)
	off += len(observed)
	binary.LittleEndian.PutUint16(buf[off:], uint16(len(alt)))
	copy(buf[off+2:], alt)
	return buf
}

func UnmarshalNATResponse(data []byte) (*NATResponsePayload, error) {
	if len(data) < 4 {
		return nil, ErrPacketTooShort
	}
	r := &NATResponsePayload{
		ProbeID: data[0],
		NATType: NATType(data[1]),
	}
	off := 2
	addrLen := int(binary.LittleEndian.Uint16(data[off:]))
	off += 2
	if len(data) < off+addrLen+2 {
		return nil, ErrPacketTooShort
	}
	r.ObservedAddr = parseAddrBytes(data[off : off+addrLen])
	off += addrLen
	altLen := int(binary.LittleEndian.Uint16(data[off:]))
	off += 2
	if len(data) < off+altLen {
		return nil, ErrPacketTooShort
	}
	if altLen > 0 {
		r.AltAddr = parseAddrBytes(data[off : off+altLen])
	}
	return r, nil
}

// addrBytes marshals a UDPAddr to "ip:port" bytes. Returns nil if addr is nil.
func addrBytes(addr *net.UDPAddr) []byte {
	if addr == nil {
		return nil
	}
	return []byte(addr.String())
}

// parseAddrBytes parses "ip:port" bytes into a UDPAddr.
func parseAddrBytes(data []byte) *net.UDPAddr {
	if len(data) == 0 {
		return nil
	}
	addr, err := net.ResolveUDPAddr("udp", string(data))
	if err != nil {
		return nil
	}
	return addr
}
