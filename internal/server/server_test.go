package server

import (
	"net"
	"testing"
	"time"
)

func newTestRoom(subnetStr string, serverIP net.IP) *Room {
	_, subnet, _ := net.ParseCIDR(subnetStr)
	r := &Room{
		clients:     make(map[[16]byte]*Client),
		addrMap:     make(map[rateKey]*Client),
		subnet:      subnet,
		serverIP:    serverIP,
		ipBitmap:    make([]uint64, 4),
		maxPlayers:  10,
		ipConnCount: make(map[connIPKey]int),
		done:        make(chan struct{}),
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

// ── Room ClientCount Tests ─────────────────────────────────────

func TestClientCount(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))

	if r.ClientCount() != 0 {
		t.Errorf("expected 0 clients, got %d", r.ClientCount())
	}

	ip := net.IPv4(10, 10, 0, 2)
	r.mu.Lock()
	r.clients[ipKey(ip)] = &Client{VirtualIP: ip, Username: "player1"}
	r.mu.Unlock()

	if r.ClientCount() != 1 {
		t.Errorf("expected 1 client, got %d", r.ClientCount())
	}
}

// ── Room CleanupStale Tests ────────────────────────────────────

func TestCleanupStale(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))
	addr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345}

	// Add stale client (> 30s old)
	oldTime := time.Now().Add(-1 * time.Minute)
	c := &Client{
		Username:  "stale",
		VirtualIP: net.IPv4(10, 10, 0, 2),
		PublicAddr: addr,
		LastSeen:  oldTime,
		auth:      authNone,
	}

	r.mu.Lock()
	r.clients[ipKey(c.VirtualIP)] = c
	r.addrMap[addrToRateKey(addr)] = c
	r.markIPUsed(c.VirtualIP)
	r.mu.Unlock()

	changed := r.CleanupStale()

	if !changed {
		t.Error("CleanupStale should return true when clients removed")
	}

	r.mu.RLock()
	_, ok := r.clients[ipKey(c.VirtualIP)]
	r.mu.RUnlock()

	if ok {
		t.Error("stale client should have been removed")
	}
}

func TestCleanupStale_NoStaleClients(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))
	addr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345}

	// Add recent client (< 30s old)
	recentTime := time.Now().Add(-10 * time.Second)
	c := &Client{
		Username:  "recent",
		VirtualIP: net.IPv4(10, 10, 0, 2),
		PublicAddr: addr,
		LastSeen:  recentTime,
		auth:      authNone,
	}

	r.mu.Lock()
	r.clients[ipKey(c.VirtualIP)] = c
	r.addrMap[addrToRateKey(addr)] = c
	r.markIPUsed(c.VirtualIP)
	r.mu.Unlock()

	changed := r.CleanupStale()

	if changed {
		t.Error("CleanupStale should return false when no clients removed")
	}

	r.mu.RLock()
	_, ok := r.clients[ipKey(c.VirtualIP)]
	r.mu.RUnlock()

	if !ok {
		t.Error("recent client should NOT be removed")
	}
}

// ── Room handleKeepAlive Tests ─────────────────────────────────

func TestRoomHandleKeepAlive(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))
	addr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345}

	oldTime := time.Now().Add(-1 * time.Minute)
	c := &Client{
		Username:  "player1",
		VirtualIP: net.IPv4(10, 10, 0, 2),
		PublicAddr: addr,
		LastSeen:  oldTime,
		auth:      authNone,
	}

	r.mu.Lock()
	r.clients[ipKey(c.VirtualIP)] = c
	r.addrMap[addrToRateKey(addr)] = c
	r.mu.Unlock()

	r.handleKeepAlive(addr)

	r.mu.RLock()
	updated := r.addrMap[addrToRateKey(addr)]
	r.mu.RUnlock()

	if updated.LastSeen.Equal(oldTime) {
		t.Error("LastSeen should be updated after keepalive")
	}
}

// ── Room handleDisconnect Tests ────────────────────────────────

func TestRoomHandleDisconnect(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))
	conn, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	r.conn = conn
	defer conn.Close()

	addr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345}
	vip := net.IPv4(10, 10, 0, 2)

	c := &Client{
		Username:  "leaver",
		VirtualIP: vip,
		PublicAddr: addr,
		LastSeen:  time.Now(),
		auth:      authNone,
	}

	r.mu.Lock()
	r.clients[ipKey(vip)] = c
	r.addrMap[addrToRateKey(addr)] = c
	r.markIPUsed(vip)
	r.mu.Unlock()

	r.handleDisconnect(addr)

	r.mu.RLock()
	_, okClient := r.clients[ipKey(vip)]
	_, okAddr := r.addrMap[addrToRateKey(addr)]
	r.mu.RUnlock()

	if okClient {
		t.Error("client should be removed after disconnect")
	}
	if okAddr {
		t.Error("addrMap entry should be removed after disconnect")
	}
}

// ── Room BuildRoomStatus Tests ─────────────────────────────────

func TestBuildRoomStatus(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))
	r.roomID = "testroom"
	r.roomPass = "secret"

	addr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345}
	c := &Client{
		Username:  "player1",
		VirtualIP: net.IPv4(10, 10, 0, 2),
		PublicAddr: addr,
		LastSeen:  time.Now(),
		RTT:       50 * time.Millisecond,
		auth:      authNone,
	}

	r.mu.Lock()
	r.clients[ipKey(c.VirtualIP)] = c
	r.mu.Unlock()

	status := r.BuildRoomStatus()

	if status.RoomID != "testroom" {
		t.Errorf("RoomID: got %q, want %q", status.RoomID, "testroom")
	}
	if status.Players != 1 {
		t.Errorf("Players: got %d, want 1", status.Players)
	}
	if !status.HasAuth {
		t.Error("HasAuth should be true when roomPass is set")
	}
	if len(status.Connections) != 1 {
		t.Errorf("Connections: got %d, want 1", len(status.Connections))
	}
	if status.Connections[0].Username != "player1" {
		t.Errorf("Username: got %q, want %q", status.Connections[0].Username, "player1")
	}
}

// ── Room Stop Tests ────────────────────────────────────────────

func TestRoomStop(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))
	r.done = make(chan struct{})
	r.regTick = time.NewTicker(time.Second)

	// Start the loop in background
	go r.regRateLimitLoop()

	// Stop should not panic
	r.Stop()

	// Verify done channel is closed
	select {
	case <-r.done:
		// expected
	default:
		t.Error("done channel should be closed after Stop()")
	}
}

// ── allocateSubnet Tests ───────────────────────────────────────

func TestAllocateSubnet(t *testing.T) {
	conn, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	defer conn.Close()

	_, defaultSubnet, _ := net.ParseCIDR("10.10.0.0/24")
	defaultRoom, _ := NewRoom(RoomConfig{
		RoomID:     "default",
		Subnet:     defaultSubnet,
		MaxPlayers: 10,
		Conn:       conn,
	})

	s := &Server{
		rooms:       map[string]*Room{"default": defaultRoom},
		defaultRoom: defaultRoom,
	}

	subnet := s.allocateSubnet()
	if subnet == nil {
		t.Fatal("allocateSubnet returned nil")
	}

	// Should be 10.10.2.0/24 (maxIdx is forced to at least 1, then +1)
	expected := net.IPv4(10, 10, 2, 0)
	if !subnet.IP.Equal(expected) {
		t.Errorf("subnet IP: got %v, want %v", subnet.IP, expected)
	}
}
