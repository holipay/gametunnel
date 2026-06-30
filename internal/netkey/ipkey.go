// Package netkey provides shared network key functions for
// both client and server packages to avoid code duplication.
package netkey

import "net"

// IPKey converts an IP address to a fixed-size [16]byte map key.
// IPv4 addresses are automatically mapped to v4-in-v6 format.
// Used as map key for O(1) peer/client lookups.
//
// Unlike ip.To16(), this avoids a heap allocation by writing the
// bytes directly, making it safe for hot paths like rate-limiting
// and peer map lookups.
func IPKey(ip net.IP) [16]byte {
	var k [16]byte
	if len(ip) == net.IPv4len {
		// v4-in-v6: 0:0:0:0:0:ffff:a.b.c.d
		k[10] = 0xff
		k[11] = 0xff
		copy(k[12:16], ip)
	} else {
		copy(k[:], ip)
	}
	return k
}

// RateKey is a fixed-size key for rate limiting, avoiding string
// allocation per packet. Uses 16-byte IP to support both IPv4
// (as v4-in-v6 mapped) and IPv6 addresses.
type RateKey struct {
	IP   [16]byte
	Port uint16
}

// AddrToRateKey converts a UDP address to a RateKey.
// IPv4 addresses are mapped to v4-in-v6 format for consistent keys.
func AddrToRateKey(addr *net.UDPAddr) RateKey {
	var k RateKey
	if len(addr.IP) == net.IPv4len {
		// v4-in-v6: 0:0:0:0:0:ffff:a.b.c.d
		k.IP[10] = 0xff
		k.IP[11] = 0xff
		copy(k.IP[12:16], addr.IP)
	} else {
		copy(k.IP[:], addr.IP)
	}
	k.Port = uint16(addr.Port)
	return k
}
