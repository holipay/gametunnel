package server

import (
	"net"
	"testing"

	"github.com/holipay/gametunnel/internal/protocol"
)

func BenchmarkHandleRelay(b *testing.B) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))
	defer close(r.done)
	conn, _ := net.ListenUDP("udp", &net.UDPAddr{})
	defer conn.Close()
	r.conn = conn
	r.sendQueue = newRateLimitedQueue(conn, nil)

	// Register two clients
	addrA := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 10001}
	addrB := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 10002}
	vipA := net.IPv4(10, 10, 0, 2).To4()
	vipB := net.IPv4(10, 10, 0, 3).To4()

	r.mu.Lock()
	r.clients[ipKey(vipA)] = &Client{VirtualIP: vipA, PublicAddr: addrA, Username: "A"}
	r.clients[ipKey(vipB)] = &Client{VirtualIP: vipB, PublicAddr: addrB, Username: "B"}
	r.addrMap[addrToRateKey(addrA)] = r.clients[ipKey(vipA)]
	r.addrMap[addrToRateKey(addrB)] = r.clients[ipKey(vipB)]
	r.mu.Unlock()

	// Build relay payload: srcIP(4) + dstIP(4) + data
	payload := make([]byte, 8+600)
	copy(payload[0:4], vipA)
	copy(payload[4:8], vipB)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.handleRelay(payload, addrA)
	}
}

func BenchmarkEncodeCheckedRelay(b *testing.B) {
	payload := make([]byte, 608) // 4 srcIP + 4 dstIP + 600 data
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		protocol.EncodeChecked(protocol.TypeData, payload)
	}
}

func BenchmarkAddrToRateKey(b *testing.B) {
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		addrToRateKey(addr)
	}
}

func BenchmarkAddrToConnIPKey(b *testing.B) {
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		addrToConnIPKey(addr)
	}
}
