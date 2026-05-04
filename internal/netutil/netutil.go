// Package netutil provides shared network helpers.
package netutil

import "net"

// IsBroadcast reports whether dst is a broadcast address for the given subnet.
// It checks both 255.255.255.255 and the subnet-directed broadcast.
func IsBroadcast(dst net.IP, subnet *net.IPNet) bool {
	ip4 := dst.To4()
	if ip4 == nil {
		return false
	}
	if ip4.Equal(net.IPv4bcast) {
		return true
	}
	if subnet != nil {
		bcast := make(net.IP, 4)
		for i := 0; i < 4; i++ {
			bcast[i] = ip4[i] | ^subnet.Mask[i]
		}
		return ip4.Equal(bcast)
	}
	return false
}
