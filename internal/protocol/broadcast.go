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

// IsIPv6Multicast reports whether dst is an IPv6 multicast address (ff00::/8).
// While the virtual subnet is IPv4, TUN devices may still receive IPv6
// multicast traffic (e.g. neighbor discovery, mDNS v6) that should be
// relayed to all peers rather than treated as unicast.
func IsIPv6Multicast(dst net.IP) bool {
	ip16 := dst.To16()
	if ip16 == nil {
		return false
	}
	// Ensure this is a native IPv6 address, not a v4-in-v6 mapped address.
	if dst.To4() != nil {
		return false
	}
	return ip16[0] == 0xff
}

// IsRelayTarget reports whether dst should be relayed to all peers.
// This includes broadcast addresses and multicast addresses.
func IsRelayTarget(dst net.IP, subnet *net.IPNet) bool {
	return IsBroadcast(dst, subnet) || IsMulticast(dst) || IsIPv6Multicast(dst)
}
