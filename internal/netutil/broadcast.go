package netutil

import "net"

// IsRelayTargetRaw checks if a raw 4-byte IPv4 address is a broadcast or multicast
// target, avoiding net.IP heap allocations on the hot relay path.
func IsRelayTargetRaw(dst [4]byte, subnet *net.IPNet) bool {
	// Broadcast: 255.255.255.255
	if dst == [4]byte{255, 255, 255, 255} {
		return true
	}
	// Subnet-directed broadcast
	if subnet != nil {
		subIP := subnet.IP.To4()
		if subIP != nil {
			var bcast [4]byte
			for i := 0; i < 4; i++ {
				bcast[i] = subIP[i] | ^subnet.Mask[i]
			}
			if dst == bcast {
				return true
			}
		}
	}
	// IPv4 multicast: 224.0.0.0 - 239.255.255.255
	if dst[0] >= 224 && dst[0] <= 239 {
		return true
	}
	return false
}
