// Package netutil provides shared network utility functions for
// both client and server packages to avoid code duplication.
package netutil

import "net"

// IPKey converts an IP address to a fixed-size [16]byte map key.
// IPv4 addresses are automatically mapped to v4-in-v6 format.
// Used as map key for O(1) peer/client lookups.
func IPKey(ip net.IP) [16]byte {
	var k [16]byte
	copy(k[:], ip.To16())
	return k
}
