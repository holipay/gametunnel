package netutil

import (
	"net"
	"testing"
)

func TestIPKey_IPv4(t *testing.T) {
	ip := net.IPv4(192, 168, 1, 1).To4()
	k := IPKey(ip)
	expected := [16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 192, 168, 1, 1}
	if k != expected {
		t.Errorf("got %v, want %v", k, expected)
	}
}

func TestIPKey_IPv6(t *testing.T) {
	ip := net.ParseIP("2408::1")
	k := IPKey(ip)
	expected := [16]byte{0x24, 0x08, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
	if k != expected {
		t.Errorf("got %v, want %v", k, expected)
	}
}

func TestIPKey_Consistency(t *testing.T) {
	ip := net.IPv4(10, 0, 0, 2).To4()
	k1 := IPKey(ip)
	k2 := IPKey(ip)
	if k1 != k2 {
		t.Error("IPKey should be deterministic")
	}
}
