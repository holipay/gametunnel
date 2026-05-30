package server

import (
	"net"
	"testing"
)

func newTestRoom(subnetStr string, serverIP net.IP) *Room {
	_, subnet, _ := net.ParseCIDR(subnetStr)
	r := &Room{
		clients:    make(map[[16]byte]*Client),
		addrMap:    make(map[rateKey]*Client),
		subnet:     subnet,
		serverIP:   serverIP,
		ipBitmap:   make([]uint64, 4),
		maxPlayers: 10,
	}
	r.markIPUsed(net.IPv4(serverIP[0], serverIP[1], serverIP[2], 0))   // network address
	r.markIPUsed(serverIP)                                                // server IP
	r.markIPUsed(net.IPv4(serverIP[0], serverIP[1], serverIP[2], 255))  // broadcast
	return r
}

func TestNextAvailableIP(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))

	// First available should be .2 (skipping .1 which is server)
	ip := r.nextAvailableIP()
	if !ip.Equal(net.IPv4(10, 10, 0, 2)) {
		t.Errorf("first IP: got %v, want 10.10.0.2", ip)
	}

	// Allocate .2, next should be .3
	ip2 := net.IPv4(10, 10, 0, 2)
	r.markIPUsed(ip2)
	r.clients[ipKey(ip2)] = &Client{VirtualIP: ip2}
	ip = r.nextAvailableIP()
	if !ip.Equal(net.IPv4(10, 10, 0, 3)) {
		t.Errorf("second IP: got %v, want 10.10.0.3", ip)
	}
}

func TestNextAvailableIPSkipsServer(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))

	// Allocate .2 through .254, skipping .1 (server)
	for i := 2; i <= 254; i++ {
		ip := net.IPv4(10, 10, 0, byte(i))
		r.markIPUsed(ip)
		r.clients[ipKey(ip)] = &Client{VirtualIP: ip}
	}

	ip := r.nextAvailableIP()
	if ip != nil {
		t.Errorf("expected nil when exhausted, got %v", ip)
	}
}

func TestNextAvailableIPExhausted(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))

	// Fill all slots (.2 through .254)
	for i := 2; i <= 254; i++ {
		ip := net.IPv4(10, 10, 0, byte(i))
		r.markIPUsed(ip)
		r.clients[ipKey(ip)] = &Client{VirtualIP: ip}
	}

	ip := r.nextAvailableIP()
	if ip != nil {
		t.Errorf("expected nil when exhausted, got %v", ip)
	}
}

func TestNextAvailableIPSkipsGaps(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))

	// Allocate .2 and .4, skip .3
	ip2 := net.IPv4(10, 10, 0, 2)
	ip4 := net.IPv4(10, 10, 0, 4)
	r.markIPUsed(ip2)
	r.markIPUsed(ip4)
	r.clients[ipKey(ip2)] = &Client{VirtualIP: ip2}
	r.clients[ipKey(ip4)] = &Client{VirtualIP: ip4}

	ip := r.nextAvailableIP()
	if !ip.Equal(net.IPv4(10, 10, 0, 3)) {
		t.Errorf("expected 10.10.0.3 (gap), got %v", ip)
	}
}

func TestAddrToRateKey(t *testing.T) {
	addr := &net.UDPAddr{IP: net.IPv4(192, 168, 1, 100), Port: 12345}
	k := addrToRateKey(addr)
	// IPv4 is mapped to v4-in-v6 format in 16-byte key
	expected := [16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 192, 168, 1, 100}
	if k.IP != expected {
		t.Errorf("IP: got %v, want %v", k.IP, expected)
	}
	if k.Port != 12345 {
		t.Errorf("Port: got %d, want 12345", k.Port)
	}
}

func TestRateLimit(t *testing.T) {
	// Create a minimal server for rate limiting
	_, subnet, _ := net.ParseCIDR("10.10.0.0/24")
	s := &Server{
		rateBuf: [2]map[rateKey]int{make(map[rateKey]int), make(map[rateKey]int)},
	}
	_ = subnet

	addr := &net.UDPAddr{IP: net.IPv4(10, 10, 0, 100), Port: 12345}

	// Should allow up to rateLimit packets
	for i := 0; i < rateLimit; i++ {
		if !s.checkRate(addr) {
			t.Errorf("packet %d should be allowed", i)
		}
	}

	// Next packet should be rejected
	if s.checkRate(addr) {
		t.Error("packet should be rejected after limit")
	}
}

func TestIPKey(t *testing.T) {
	// IPv4 should map to v4-in-v6
	ip4 := net.IPv4(10, 10, 0, 1)
	k4 := ipKey(ip4)
	expected4 := [16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 10, 10, 0, 1}
	if k4 != expected4 {
		t.Errorf("IPv4 key: got %v, want %v", k4, expected4)
	}

	// Same IP as 16-byte should produce same key
	ip4as16 := ip4.To16()
	k4as16 := ipKey(ip4as16)
	if k4as16 != k4 {
		t.Errorf("IPv4-as-16 key mismatch: %v != %v", k4as16, k4)
	}
}
