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

func TestIsBroadcast(t *testing.T) {
	_, subnet, _ := net.ParseCIDR("10.10.0.0/24")

	tests := []struct {
		name string
		ip   net.IP
		want bool
	}{
		{"limited broadcast", net.IPv4bcast, true},
		{"subnet-directed broadcast", net.IPv4(10, 10, 0, 255), true},
		{"subnet network address", net.IPv4(10, 10, 0, 0), false},
		{"regular unicast", net.IPv4(10, 10, 0, 42), false},
		{"other subnet unicast", net.IPv4(10, 20, 0, 1), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsBroadcast(tt.ip, subnet)
			if got != tt.want {
				t.Errorf("IsBroadcast(%s, %s) = %v, want %v", tt.ip, subnet, got, tt.want)
			}
		})
	}
}

func TestIsRelayTarget_Broadcast(t *testing.T) {
	_, subnet, _ := net.ParseCIDR("10.10.0.0/24")

	tests := []struct {
		name string
		ip   net.IP
		want bool
	}{
		{"limited broadcast", net.IPv4bcast, true},
		{"subnet-directed broadcast", net.IPv4(10, 10, 0, 255), true},
		{"IPv4 multicast", net.IPv4(224, 0, 0, 1), true},
		{"IPv6 multicast", net.ParseIP("ff02::1"), true},
		{"regular unicast", net.IPv4(10, 10, 0, 42), false},
		{"other subnet", net.IPv4(10, 20, 0, 1), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsRelayTarget(tt.ip, subnet)
			if got != tt.want {
				t.Errorf("IsRelayTarget(%s, %s) = %v, want %v", tt.ip, subnet, got, tt.want)
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
