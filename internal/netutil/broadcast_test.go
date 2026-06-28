package netutil

import (
	"net"
	"testing"
)

func TestIsIPv6Multicast(t *testing.T) {
	tests := []struct {
		name string
		ip   net.IP
		want bool
	}{
		{"IPv6 all-nodes ff02::1", net.ParseIP("ff02::1"), true},
		{"IPv6 mDNS ff02::fb", net.ParseIP("ff02::fb"), true},
		{"IPv6 solicited-node ff02::1:ff00:1", net.ParseIP("ff02::1:ff00:1"), true},
		{"IPv6 global unicast", net.ParseIP("2408:abcd::1"), false},
		{"IPv6 loopback", net.IPv6loopback, false},
		{"IPv4 multicast (not IPv6)", net.IPv4(224, 0, 0, 251), false},
		{"IPv4 broadcast", net.IPv4bcast, false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsIPv6Multicast(tt.ip); got != tt.want {
				t.Errorf("IsIPv6Multicast(%s) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
}

func TestIsRelayTarget_IPv6Multicast(t *testing.T) {
	_, subnet, _ := net.ParseCIDR("10.10.0.0/24")

	if !IsRelayTarget(net.ParseIP("ff02::1"), subnet) {
		t.Error("ff02::1 should be a relay target")
	}

	if IsRelayTarget(net.ParseIP("2408:abcd::1"), subnet) {
		t.Error("2408:abcd::1 should not be a relay target")
	}
}
