package netutil

import "net"

func IsBroadcast(dst net.IP, subnet *net.IPNet) bool {
	ip4 := dst.To4()
	if ip4 == nil {
		return false
	}
	if ip4.Equal(net.IPv4bcast) {
		return true
	}
	if subnet != nil {
		subIP := subnet.IP.To4()
		if subIP == nil {
			return false
		}
		var bcast [4]byte
		for i := 0; i < 4; i++ {
			bcast[i] = subIP[i] | ^subnet.Mask[i]
		}
		return ip4.Equal(bcast[:])
	}
	return false
}

func IsMulticast(dst net.IP) bool {
	ip4 := dst.To4()
	if ip4 == nil {
		return false
	}
	return ip4[0] >= 224 && ip4[0] <= 239
}

func IsIPv6Multicast(dst net.IP) bool {
	ip16 := dst.To16()
	if ip16 == nil {
		return false
	}
	if dst.To4() != nil {
		return false
	}
	return ip16[0] == 0xff
}

func IsRelayTarget(dst net.IP, subnet *net.IPNet) bool {
	return IsBroadcast(dst, subnet) || IsMulticast(dst) || IsIPv6Multicast(dst)
}

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
