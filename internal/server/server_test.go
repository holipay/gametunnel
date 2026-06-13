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
		regBuf:      [2]map[string]int{make(map[string]int), make(map[string]int)},
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

func TestAllocateSubnet_MultipleRooms(t *testing.T) {
	conn, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	defer conn.Close()

	_, defaultSubnet, _ := net.ParseCIDR("10.10.0.0/24")
	defaultRoom, _ := NewRoom(RoomConfig{
		RoomID:     "default",
		Subnet:     defaultSubnet,
		MaxPlayers: 10,
		Conn:       conn,
	})

	_, room2Subnet, _ := net.ParseCIDR("10.10.5.0/24")
	room2, _ := NewRoom(RoomConfig{
		RoomID:     "room2",
		Subnet:     room2Subnet,
		MaxPlayers: 10,
		Conn:       conn,
	})

	s := &Server{
		rooms: map[string]*Room{
			"default": defaultRoom,
			"room2":   room2,
		},
		defaultRoom: defaultRoom,
	}

	subnet := s.allocateSubnet()
	if subnet == nil {
		t.Fatal("allocateSubnet returned nil")
	}

	// Should be 10.10.6.0/24 (maxIdx=5, then +1)
	expected := net.IPv4(10, 10, 6, 0)
	if !subnet.IP.Equal(expected) {
		t.Errorf("subnet IP: got %v, want %v", subnet.IP, expected)
	}
}

func TestAllocateSubnet_NilDefaultRoom(t *testing.T) {
	s := &Server{
		rooms:       map[string]*Room{},
		defaultRoom: nil,
	}

	subnet := s.allocateSubnet()
	if subnet != nil {
		t.Errorf("expected nil for nil defaultRoom, got %v", subnet)
	}
}

// ── formatDuration Tests ───────────────────────────────────────

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{90 * time.Second, "1m30s"},
		{1 * time.Hour, "1h0m"},
		{25 * time.Hour, "1d1h"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := formatDuration(tt.d)
			if got != tt.want {
				t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}

// ── connIPKey Tests ────────────────────────────────────────────

func TestAddrToConnIPKey(t *testing.T) {
	addr := &net.UDPAddr{IP: net.IPv4(192, 168, 1, 100), Port: 12345}
	k := addrToConnIPKey(addr)

	expected := connIPKey{192, 168, 1, 100}
	if k != expected {
		t.Errorf("got %v, want %v", k, expected)
	}
}

func TestAddrToConnIPKey_IPv6(t *testing.T) {
	addr := &net.UDPAddr{IP: net.ParseIP("2408::1"), Port: 12345}
	k := addrToConnIPKey(addr)

	// IPv6 should return zero key
	expected := connIPKey{}
	if k != expected {
		t.Errorf("got %v, want %v for IPv6", k, expected)
	}
}

// ── Room SnapshotState Tests ───────────────────────────────────

func TestSnapshotState(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))
	r.roomID = "testroom"

	addr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345}
	c := &Client{
		Username:  "player1",
		VirtualIP: net.IPv4(10, 10, 0, 2),
		PublicAddr: addr,
		LastSeen:  time.Now(),
		auth:      authNone,
	}

	r.mu.Lock()
	r.clients[ipKey(c.VirtualIP)] = c
	r.markIPUsed(c.VirtualIP)
	r.mu.Unlock()

	state := r.SnapshotState()

	if state.Version != stateVersion {
		t.Errorf("Version: got %d, want %d", state.Version, stateVersion)
	}
	if state.Subnet != "10.10.0.0/24" {
		t.Errorf("Subnet: got %q", state.Subnet)
	}
	if len(state.Clients) != 1 {
		t.Errorf("Clients: got %d, want 1", len(state.Clients))
	}

	entry, ok := state.Clients["10.10.0.2"]
	if !ok {
		t.Fatal("expected client 10.10.0.2 in snapshot")
	}
	if entry.Username != "player1" {
		t.Errorf("Username: got %q", entry.Username)
	}
}

func TestSnapshotState_SkipsAuthChallenge(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))

	addr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345}
	c := &Client{
		Username:  "challenger",
		VirtualIP: net.IPv4(10, 10, 0, 2),
		PublicAddr: addr,
		LastSeen:  time.Now(),
		auth:      authChallengeSent, // not fully authenticated
	}

	r.mu.Lock()
	r.clients[ipKey(c.VirtualIP)] = c
	r.mu.Unlock()

	state := r.SnapshotState()

	if len(state.Clients) != 0 {
		t.Errorf("expected 0 clients (auth challenge), got %d", len(state.Clients))
	}
}

// ── Room IP Bitmap Tests ───────────────────────────────────────

func TestMarkIPUsedAndFree(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))

	ip := net.IPv4(10, 10, 0, 50)

	// Mark used
	r.markIPUsed(ip)
	octet := ip.To4()[3]
	if r.ipBitmap[octet/64]&(1<<(octet%64)) == 0 {
		t.Error("IP should be marked as used")
	}

	// Mark free
	r.markIPFree(ip)
	if r.ipBitmap[octet/64]&(1<<(octet%64)) != 0 {
		t.Error("IP should be marked as free")
	}
}

// ── Room checkRegRate Tests ────────────────────────────────────

func TestCheckRegRate(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))
	r.maxRegPerIP = 3

	ip := "1.2.3.4"

	// Should allow up to maxRegPerIP
	for i := 0; i < 3; i++ {
		if !r.checkRegRate(ip) {
			t.Errorf("request %d should be allowed", i)
		}
	}

	// Next should be rejected
	if r.checkRegRate(ip) {
		t.Error("request should be rejected after limit")
	}
}

// ── Room getAuthKey Tests ──────────────────────────────────────

func TestGetAuthKey_NilPassword(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))
	r.roomPass = ""

	key := r.getAuthKey("testroom")
	if key != nil {
		t.Error("expected nil key for empty password")
	}
}

func TestGetAuthKey_WithPassword(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))
	r.roomPass = "secret"

	key := r.getAuthKey("testroom")
	if key == nil {
		t.Fatal("expected non-nil key")
	}
	if len(key) != 32 {
		t.Errorf("key length: got %d, want 32", len(key))
	}

	// Should be cached
	key2 := r.getAuthKey("testroom")
	if len(key2) != len(key) {
		t.Error("cached key should have same length")
	}
}

// ── markDirty Tests ────────────────────────────────────────────

func TestMarkDirty(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))

	called := false
	r.onDirty = func() { called = true }

	r.markDirty()

	if !called {
		t.Error("onDirty should be called")
	}
}

func TestMarkDirty_NilCallback(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))
	r.onDirty = nil

	// Should not panic
	r.markDirty()
}
