package netutil

import (
	"net"
	"testing"
)

func TestIsRelayTargetRaw(t *testing.T) {
	_, subnet, _ := net.ParseCIDR("10.10.0.0/24")

	tests := []struct {
		name string
		ip   [4]byte
		want bool
	}{
		{"limited broadcast", [4]byte{255, 255, 255, 255}, true},
		{"subnet-directed broadcast", [4]byte{10, 10, 0, 255}, true},
		{"IPv4 multicast", [4]byte{224, 0, 0, 1}, true},
		{"regular unicast", [4]byte{10, 10, 0, 42}, false},
		{"other subnet", [4]byte{10, 20, 0, 1}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRelayTargetRaw(tt.ip, subnet); got != tt.want {
				t.Errorf("IsRelayTargetRaw(%v, %s) = %v, want %v", tt.ip, subnet, got, tt.want)
			}
		})
	}
}
