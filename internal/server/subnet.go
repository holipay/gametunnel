package server

import "net"

// allocateSubnet finds an unused /24 subnet for a new room.
// Uses 10.10.{room_index}.0/24 starting from 10.10.2.0.
// Skips subnets that overlap with any local network interface.
func (s *Server) allocateSubnet() *net.IPNet {
	// Derive room subnets from the base subnet prefix.
	// e.g. server -subnet 192.168.1.0/24 → rooms get 192.168.2.0/24, 192.168.3.0/24, ...
	var baseIP net.IP
	if s.defaultRoom != nil {
		baseIP = s.defaultRoom.subnet.IP.To4()
	} else if s.baseSubnet != nil {
		baseIP = s.baseSubnet.IP.To4()
	}
	if baseIP == nil {
		return nil
	}

	// Find the highest used 3rd octet
	maxIdx := int(baseIP[2])
	if maxIdx < 1 {
		maxIdx = 1
	}
	for _, room := range s.rooms {
		octet := int(room.subnet.IP.To4()[2])
		if octet > maxIdx {
			maxIdx = octet
		}
	}

	// Scan for the next available subnet, skipping those that overlap
	// with local interfaces or already-used room subnets.
	for nextIdx := maxIdx + 1; nextIdx <= 254; nextIdx++ {
		candidate := &net.IPNet{
			IP:   net.IPv4(baseIP[0], baseIP[1], byte(nextIdx), 0),
			Mask: net.CIDRMask(24, 32),
		}
		if s.subnetOverlapsAny(candidate, s.localSubnets) {
			continue
		}
		return candidate
	}
	return nil // no more subnets
}

// getLocalSubnets returns all /24+ subnets assigned to local network interfaces.
func getLocalSubnets() []*net.IPNet {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var subnets []*net.IPNet
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ones, bits := ipNet.Mask.Size()
			if bits == 32 && ones >= 24 {
				subnets = append(subnets, ipNet)
			}
		}
	}
	return subnets
}

// subnetOverlapsAny checks if candidate overlaps with any subnet in the list.
func (s *Server) subnetOverlapsAny(candidate *net.IPNet, others []*net.IPNet) bool {
	for _, other := range others {
		if candidate.IP.Equal(other.IP) {
			return true
		}
		// Check if either network contains the other's IP
		if candidate.Contains(other.IP) || other.Contains(candidate.IP) {
			return true
		}
	}
	return false
}
