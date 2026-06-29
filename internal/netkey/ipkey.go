// Package netutil provides shared network utility functions for
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
