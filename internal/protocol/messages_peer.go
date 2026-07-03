package protocol

import (
	"encoding/binary"
	"log"
	"net"
	"strconv"
	"sync"
)

// ── Peer Info ──────────────────────────────────────────────────

// PeerInfoEntry describes a single peer in the peer list.
type PeerInfoEntry struct {
	VirtualIP  net.IP
	PublicAddr *net.UDPAddr
	Username   string
	NATType    NATType // peer's NAT type (0 = unknown, from NAT probe)
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

// AddrStrLen returns the byte length of "ip:port" without allocating a string.
func AddrStrLen(addr *net.UDPAddr) int {
	if addr.IP.To4() != nil {
		// IPv4: "1.2.3.4:12345" = 4+3+1+1 = up to 21 bytes
		n := 4 + 3 + 1 // ip bytes + 3 dots + ":" + min port
		p := addr.Port
		if p >= 10 { n++ }
		if p >= 100 { n++ }
		if p >= 1000 { n++ }
		if p >= 10000 { n++ }
		return n
	}
	// IPv6: "[::1]:12345" — use addr.String() length as upper bound
	// We'll write it directly in AppendAddrStr
	return len(addr.String())
}

// AppendAddrStr appends "ip:port" to buf without allocating an intermediate string.
func AppendAddrStr(buf []byte, addr *net.UDPAddr) []byte {
	if ip4 := addr.IP.To4(); ip4 != nil {
		buf = strconv.AppendInt(buf, int64(ip4[0]), 10)
		buf = append(buf, '.')
		buf = strconv.AppendInt(buf, int64(ip4[1]), 10)
		buf = append(buf, '.')
		buf = strconv.AppendInt(buf, int64(ip4[2]), 10)
		buf = append(buf, '.')
		buf = strconv.AppendInt(buf, int64(ip4[3]), 10)
		buf = append(buf, ':')
		buf = strconv.AppendInt(buf, int64(addr.Port), 10)
		return buf
	}
	// IPv6: write "[ip]:port" directly
	buf = append(buf, '[')
	buf = append(buf, addr.IP.String()...)
	buf = append(buf, ']', ':')
	buf = strconv.AppendInt(buf, int64(addr.Port), 10)
	return buf
}

func (p *PeerInfoPayload) Marshal() []byte {
	// Pre-calculate total size to avoid multiple allocations
	total := 2 // peer count (2 bytes)
	hasNATInfo := false
	for _, peer := range p.Peers {
		total += 4 // VirtualIP (4 bytes IPv4)
		total += 2 // addr length prefix
		if peer.PublicAddr != nil {
			total += AddrStrLen(peer.PublicAddr)
		}
		total += 2 // username length prefix
		total += len(peer.Username)
		if peer.NATType != 0 {
			hasNATInfo = true
		}
	}
	// Trailing NAT type section: 1 byte per peer (only if any peer has NAT info)
	if hasNATInfo {
		total += len(p.Peers)
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
		addrStart := len(buf)
		buf = append(buf, 0, 0) // placeholder for addr length
		if peer.PublicAddr != nil {
			buf = AppendAddrStr(buf, peer.PublicAddr)
		}
		addrLen := len(buf) - addrStart - 2
		buf[addrStart] = byte(addrLen)
		buf[addrStart+1] = byte(addrLen >> 8)
		userStart := len(buf)
		buf = append(buf, 0, 0) // placeholder for username length
		buf = append(buf, []byte(peer.Username)...)
		userLen := len(buf) - userStart - 2
		buf[userStart] = byte(userLen)
		buf[userStart+1] = byte(userLen >> 8)
	}
	// Trailing NAT type section (backward compatible: old clients stop at count boundary)
	if hasNATInfo {
		for _, peer := range p.Peers {
			buf = append(buf, byte(peer.NATType))
		}
	}
	return buf
}

func UnmarshalPeerInfo(data []byte) (*PeerInfoPayload, error) {
	if len(data) < 2 {
		return nil, ErrPacketTooShort
	}
	count := int(binary.LittleEndian.Uint16(data[0:2]))
	if count > 256 {
		return nil, ErrTooManyPeers
	}
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
					} else {
						log.Printf("[protocol] malformed peer IP: %q", host)
					}
				} else {
					log.Printf("[protocol] malformed peer port: %q", portStr)
				}
			} else {
				log.Printf("[protocol] malformed peer address: %q", addrStr)
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
	// Trailing NAT type section: 1 byte per peer if present
	if off < len(data) {
		natLen := len(data) - off
		for i := 0; i < count && i < natLen; i++ {
			payload.Peers[i].NATType = NATType(data[off+i])
		}
	}
	return payload, nil
}
