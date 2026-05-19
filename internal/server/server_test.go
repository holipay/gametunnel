package server

import (
	"net"
	"testing"
)

func newTestServer(subnetStr string, serverIP net.IP) *Server {
	_, subnet, _ := net.ParseCIDR(subnetStr)
	s := &Server{
		clients:    make(map[[16]byte]*Client),
		addrMap:    make(map[rateKey]*Client),
		subnet:     subnet,
		serverIP:   serverIP,
		ipBitmap:   make([]uint64, 4),
		maxPlayers: 10,
	}
	s.markIPUsed(net.IPv4(serverIP[0], serverIP[1], serverIP[2], 0))   // network address
	s.markIPUsed(serverIP)                                                // server IP
	s.markIPUsed(net.IPv4(serverIP[0], serverIP[1], serverIP[2], 255))  // broadcast
	return s
}

func TestNextAvailableIP(t *testing.T) {
	s := newTestServer("10.10.0.0/24", net.IPv4(10, 10, 0, 1))

	// First available should be .2 (skipping .1 which is server)
	ip := s.nextAvailableIP()
	if !ip.Equal(net.IPv4(10, 10, 0, 2)) {
		t.Errorf("first IP: got %v, want 10.10.0.2", ip)
	}

	// Allocate .2, next should be .3
	ip2 := net.IPv4(10, 10, 0, 2)
	s.markIPUsed(ip2)
	s.clients[ipKey(ip2)] = &Client{VirtualIP: ip2}
	ip = s.nextAvailableIP()
	if !ip.Equal(net.IPv4(10, 10, 0, 3)) {
		t.Errorf("second IP: got %v, want 10.10.0.3", ip)
	}
}

func TestNextAvailableIPSkipsServer(t *testing.T) {
	s := newTestServer("10.10.0.0/24", net.IPv4(10, 10, 0, 1))

	// Allocate .2 through .254, skipping .1 (server)
	for i := 2; i <= 254; i++ {
		ip := net.IPv4(10, 10, 0, byte(i))
		s.markIPUsed(ip)
		s.clients[ipKey(ip)] = &Client{VirtualIP: ip}
	}

	ip := s.nextAvailableIP()
	if ip != nil {
		t.Errorf("expected nil when exhausted, got %v", ip)
	}
}

func TestNextAvailableIPExhausted(t *testing.T) {
	s := newTestServer("10.10.0.0/24", net.IPv4(10, 10, 0, 1))

	// Fill all slots (.2 through .254)
	for i := 2; i <= 254; i++ {
		ip := net.IPv4(10, 10, 0, byte(i))
		s.markIPUsed(ip)
		s.clients[ipKey(ip)] = &Client{VirtualIP: ip}
	}

	ip := s.nextAvailableIP()
	if ip != nil {
		t.Errorf("expected nil when exhausted, got %v", ip)
	}
}

func TestNextAvailableIPSkipsGaps(t *testing.T) {
	s := newTestServer("10.10.0.0/24", net.IPv4(10, 10, 0, 1))

	// Allocate .2 and .4, skip .3
	ip2 := net.IPv4(10, 10, 0, 2)
	ip4 := net.IPv4(10, 10, 0, 4)
	s.markIPUsed(ip2)
	s.markIPUsed(ip4)
	s.clients[ipKey(ip2)] = &Client{VirtualIP: ip2}
	s.clients[ipKey(ip4)] = &Client{VirtualIP: ip4}

	ip := s.nextAvailableIP()
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

func TestCheckRateLimit(t *testing.T) {
	s := newTestServer("10.10.0.0/24", net.IPv4(10, 10, 0, 1))
	s.rateBuf = [2]map[rateKey]int{make(map[rateKey]int), make(map[rateKey]int)}
	addr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1000}

	// Should pass up to rateLimit
	for i := 0; i < rateLimit; i++ {
		if !s.checkRate(addr) {
			t.Fatalf("should pass at attempt %d", i+1)
		}
	}

	// Next one should be rejected
	if s.checkRate(addr) {
		t.Fatal("should be rejected after rate limit")
	}

	// Different address should pass
	addr2 := &net.UDPAddr{IP: net.IPv4(5, 6, 7, 8), Port: 2000}
	if !s.checkRate(addr2) {
		t.Fatal("different address should pass")
	}
}

func TestCheckRegRate(t *testing.T) {
	s := newTestServer("10.10.0.0/24", net.IPv4(10, 10, 0, 1))
	s.regBuf = [2]map[string]int{make(map[string]int), make(map[string]int)}
	s.maxRegPerIP = 3

	ip := "1.2.3.4"
	for i := 0; i < 3; i++ {
		if !s.checkRegRate(ip) {
			t.Fatalf("should pass at attempt %d", i+1)
		}
	}
	if s.checkRegRate(ip) {
		t.Fatal("should be rejected after reg rate limit")
	}
}

// ── IPv6 Tests ────────────────────────────────────────────────

func TestIpKey_IPv6(t *testing.T) {
	ip := net.ParseIP("2408:abcd::1")
	key := ipKey(ip)
	expected := [16]byte{0x24, 0x08, 0xab, 0xcd, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
	if key != expected {
		t.Errorf("IPv6 ipKey: got %v, want %v", key, expected)
	}
}

func TestIpKey_IPv4v6Consistency(t *testing.T) {
	ip4 := net.IPv4(10, 0, 0, 1).To4()
	ip16 := net.IP{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 10, 0, 0, 1}
	if ipKey(ip4) != ipKey(ip16) {
		t.Error("IPv4 and v4-in-v6 mapped should produce the same ipKey")
	}
}

func TestAddrToRateKey_IPv6(t *testing.T) {
	ip := net.ParseIP("2408:abcd::1")
	addr := &net.UDPAddr{IP: ip, Port: 4700}
	k := addrToRateKey(addr)
	expected := [16]byte{0x24, 0x08, 0xab, 0xcd, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
	if k.IP != expected {
		t.Errorf("IPv6 rateKey IP: got %v, want %v", k.IP, expected)
	}
	if k.Port != 4700 {
		t.Errorf("Port: got %d, want 4700", k.Port)
	}
}

func TestAddrToRateKey_IPv4Mapped(t *testing.T) {
	addr4 := &net.UDPAddr{IP: net.IPv4(192, 168, 1, 100), Port: 12345}
	addr46 := &net.UDPAddr{IP: net.IP{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 192, 168, 1, 100}, Port: 12345}
	if addrToRateKey(addr4) != addrToRateKey(addr46) {
		t.Error("IPv4 and v4-in-v6 should produce the same rateKey")
	}
}
