package protocol

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

// IsMulticast reports whether dst is an IPv4 multicast address (224.0.0.0/4).
// LAN games like StarCraft use mDNS (224.0.0.251:5353) for discovery.
func IsMulticast(dst net.IP) bool {
	ip4 := dst.To4()
	if ip4 == nil {
		return false
	}
	return ip4[0] >= 224 && ip4[0] <= 239
}

// IsRelayTarget reports whether dst should be relayed to all peers.
// This includes broadcast addresses and multicast addresses.
func IsRelayTarget(dst net.IP, subnet *net.IPNet) bool {
	return IsBroadcast(dst, subnet) || IsMulticast(dst)
}
