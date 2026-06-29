package server

import (
	"net"
	"testing"
	"time"

	"github.com/holipay/gametunnel/internal/protocol"
)

func newTestRoom(subnetStr string, serverIP net.IP) *Room {
	_, subnet, _ := net.ParseCIDR(subnetStr)
	// Create a dummy conn for the send queue (tests don't actually send)
	conn, _ := net.ListenUDP("udp", &net.UDPAddr{})
	sq := newRateLimitedQueue(conn, nil, nil)
	r := &Room{
		clients:     make(map[[16]byte]*Client),
		addrMap:     make(map[rateKey]*Client),
		subnet:      subnet,
		serverIP:    serverIP,
		sendQueue:   sq,
		ipBitmap:    make([]uint64, 4),
		maxPlayers:  10,
		ipConnCount: make(map[connIPKey]int),
		regBuf:      [2]map[connIPKey]int{make(map[connIPKey]int), make(map[connIPKey]int)},
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
		rateShards: newRateShardsArray(),
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
		auth:      authNone,
	}
	c.SetLastSeen(oldTime)

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
		auth:      authNone,
	}
	c.SetLastSeen(recentTime)

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
		auth:      authNone,
	}
	c.SetLastSeen(oldTime)

	r.mu.Lock()
	r.clients[ipKey(c.VirtualIP)] = c
	r.addrMap[addrToRateKey(addr)] = c
	r.mu.Unlock()

	r.handleKeepAlive(addr)

	r.mu.RLock()
	updated := r.addrMap[addrToRateKey(addr)]
	r.mu.RUnlock()

	if updated.GetLastSeen().Equal(oldTime) {
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
		auth:      authNone,
	}
	c.SetLastSeen(time.Now())

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
		RTT:       50 * time.Millisecond,
		auth:      authNone,
	}
	c.SetLastSeen(time.Now())

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

	expected := connIPKey{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 192, 168, 1, 100}
	if k != expected {
		t.Errorf("got %v, want %v", k, expected)
	}
}

func TestAddrToConnIPKey_IPv6(t *testing.T) {
	addr := &net.UDPAddr{IP: net.ParseIP("2408::1"), Port: 12345}
	k := addrToConnIPKey(addr)

	// IPv6 should return the full 16-byte key
	expected := connIPKey{0x24, 0x08, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
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
		auth:      authNone,
	}
	c.SetLastSeen(time.Now())

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
		auth:      authChallengeSent, // not fully authenticated
	}
	c.SetLastSeen(time.Now())

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

	addr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345}

	// Should allow up to maxRegPerIP
	for i := 0; i < 3; i++ {
		if !r.checkRegRate(addr) {
			t.Errorf("request %d should be allowed", i)
		}
	}

	// Next should be rejected
	if r.checkRegRate(addr) {
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

// ── PingStats Tests ────────────────────────────────────────────

func TestPingStats_NoData(t *testing.T) {
	c := &Client{}
	loss, jitter := c.PingStats()
	if loss != 0 {
		t.Errorf("loss: got %f, want 0", loss)
	}
	if jitter != 0 {
		t.Errorf("jitter: got %v, want 0", jitter)
	}
}

func TestPingStats_AllReceived(t *testing.T) {
	c := &Client{}
	// Simulate 5 successful pings
	for i := 0; i < 5; i++ {
		c.pingHistory[c.pingIdx%pingHistorySize] = time.Duration(50+i*5) * time.Millisecond
		c.pingIdx++
	}

	loss, _ := c.PingStats()
	if loss != 0 {
		t.Errorf("loss: got %f, want 0", loss)
	}
}

func TestPingStats_AllMissed(t *testing.T) {
	c := &Client{}
	// Simulate 5 missed pings (RTT = 0)
	for i := 0; i < 5; i++ {
		c.pingHistory[c.pingIdx%pingHistorySize] = 0
		c.pingIdx++
	}

	loss, _ := c.PingStats()
	if loss != 1.0 {
		t.Errorf("loss: got %f, want 1.0", loss)
	}
}

func TestPingStats_MixedResults(t *testing.T) {
	c := &Client{}
	// 3 received, 2 missed
	rtts := []time.Duration{
		50 * time.Millisecond,
		0, // missed
		55 * time.Millisecond,
		0, // missed
		60 * time.Millisecond,
	}
	for _, rtt := range rtts {
		c.pingHistory[c.pingIdx%pingHistorySize] = rtt
		c.pingIdx++
	}

	loss, _ := c.PingStats()
	expectedLoss := 1.0 - 3.0/5.0
	if loss != expectedLoss {
		t.Errorf("loss: got %f, want %f", loss, expectedLoss)
	}
}

func TestPingStats_RingBufferWraparound(t *testing.T) {
	c := &Client{}
	// Fill beyond pingHistorySize (12)
	for i := 0; i < 15; i++ {
		c.pingHistory[c.pingIdx%pingHistorySize] = time.Duration(50) * time.Millisecond
		c.pingIdx++
	}

	loss, _ := c.PingStats()
	if loss != 0 {
		t.Errorf("loss: got %f, want 0", loss)
	}
}

// ── HandlePacket Tests ─────────────────────────────────────────

func TestHandlePacket_KeepAlive(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))
	conn, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	r.conn = conn
	defer conn.Close()

	addr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345}
	c := &Client{
		Username:  "player1",
		VirtualIP: net.IPv4(10, 10, 0, 2),
		PublicAddr: addr,
		auth:      authNone,
	}
	c.SetLastSeen(time.Now().Add(-1 * time.Minute))

	r.mu.Lock()
	r.clients[ipKey(c.VirtualIP)] = c
	r.addrMap[addrToRateKey(addr)] = c
	r.mu.Unlock()

	r.HandlePacket(protocol.TypeKeepAlive, []byte{}, addr)

	r.mu.RLock()
	updated := r.addrMap[addrToRateKey(addr)]
	r.mu.RUnlock()

	if updated.GetLastSeen().Before(time.Now().Add(-10 * time.Second)) {
		t.Error("LastSeen should be updated after keepalive")
	}
}

func TestHandlePacket_PeerRequest(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))
	conn, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	r.conn = conn
	defer conn.Close()

	addr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345}
	c := &Client{
		Username:  "player1",
		VirtualIP: net.IPv4(10, 10, 0, 2),
		PublicAddr: addr,
		auth:      authNone,
	}
	c.SetLastSeen(time.Now())

	r.mu.Lock()
	r.clients[ipKey(c.VirtualIP)] = c
	r.addrMap[addrToRateKey(addr)] = c
	r.mu.Unlock()

	// Should not panic
	r.HandlePacket(protocol.TypePeerRequest, []byte{}, addr)
}

func TestHandlePacket_Disconnect(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))
	conn, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	r.conn = conn
	defer conn.Close()

	addr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345}
	c := &Client{
		Username:  "leaver",
		VirtualIP: net.IPv4(10, 10, 0, 2),
		PublicAddr: addr,
		auth:      authNone,
	}
	c.SetLastSeen(time.Now())

	r.mu.Lock()
	r.clients[ipKey(c.VirtualIP)] = c
	r.addrMap[addrToRateKey(addr)] = c
	r.markIPUsed(c.VirtualIP)
	r.mu.Unlock()

	r.HandlePacket(protocol.TypeDisconnect, []byte{}, addr)

	r.mu.RLock()
	_, ok := r.clients[ipKey(c.VirtualIP)]
	r.mu.RUnlock()

	if ok {
		t.Error("client should be removed after disconnect")
	}
}

// ── handleRelay Tests ──────────────────────────────────────────

func TestHandleRelay_Unicast(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))
	conn, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	r.conn = conn
	defer conn.Close()

	senderAddr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1000}
	receiverAddr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 5), Port: 2000}

	sender := &Client{
		Username:  "sender",
		VirtualIP: net.IPv4(10, 10, 0, 2),
		PublicAddr: senderAddr,
		auth:      authNone,
	}
	sender.SetLastSeen(time.Now())
	receiver := &Client{
		Username:  "receiver",
		VirtualIP: net.IPv4(10, 10, 0, 3),
		PublicAddr: receiverAddr,
		auth:      authNone,
	}
	receiver.SetLastSeen(time.Now())

	r.mu.Lock()
	r.clients[ipKey(sender.VirtualIP)] = sender
	r.clients[ipKey(receiver.VirtualIP)] = receiver
	r.addrMap[addrToRateKey(senderAddr)] = sender
	r.addrMap[addrToRateKey(receiverAddr)] = receiver
	r.mu.Unlock()

	// Build relay payload: srcIP(4) + dstIP(4) + data
	payload := make([]byte, 4+4+5)
	copy(payload[0:4], sender.VirtualIP.To4())
	copy(payload[4:8], receiver.VirtualIP.To4())
	copy(payload[8:], []byte{0x01, 0x02, 0x03, 0x04, 0x05})

	r.handleRelay(payload, senderAddr)

	if r.totalPacketsRelay.Load() != 1 {
		t.Errorf("totalPacketsRelay: got %d, want 1", r.totalPacketsRelay.Load())
	}
}

func TestHandleRelay_Broadcast(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))
	conn, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	r.conn = conn
	defer conn.Close()

	senderAddr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1000}
	receiver1Addr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 5), Port: 2000}
	receiver2Addr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 6), Port: 3000}

	sender := &Client{
		Username:  "sender",
		VirtualIP: net.IPv4(10, 10, 0, 2),
		PublicAddr: senderAddr,
		auth:      authNone,
	}
	sender.SetLastSeen(time.Now())
	receiver1 := &Client{
		Username:  "receiver1",
		VirtualIP: net.IPv4(10, 10, 0, 3),
		PublicAddr: receiver1Addr,
		auth:      authNone,
	}
	receiver1.SetLastSeen(time.Now())
	receiver2 := &Client{
		Username:  "receiver2",
		VirtualIP: net.IPv4(10, 10, 0, 4),
		PublicAddr: receiver2Addr,
		auth:      authNone,
	}
	receiver2.SetLastSeen(time.Now())

	r.mu.Lock()
	r.clients[ipKey(sender.VirtualIP)] = sender
	r.clients[ipKey(receiver1.VirtualIP)] = receiver1
	r.clients[ipKey(receiver2.VirtualIP)] = receiver2
	r.addrMap[addrToRateKey(senderAddr)] = sender
	r.addrMap[addrToRateKey(receiver1Addr)] = receiver1
	r.addrMap[addrToRateKey(receiver2Addr)] = receiver2
	r.mu.Unlock()

	// Build broadcast payload (dstIP = 255.255.255.255)
	payload := make([]byte, 4+4+3)
	copy(payload[0:4], sender.VirtualIP.To4())
	copy(payload[4:8], net.IPv4(255, 255, 255, 255).To4())
	copy(payload[8:], []byte{0x01, 0x02, 0x03})

	r.handleRelay(payload, senderAddr)

	if r.totalPacketsRelay.Load() != 1 {
		t.Errorf("totalPacketsRelay: got %d, want 1", r.totalPacketsRelay.Load())
	}
}

func TestHandleRelay_ShortPayload(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))

	// Payload too short (< 8 bytes)
	r.handleRelay([]byte{0x01, 0x02}, &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1000})

	if r.totalPacketsRelay.Load() != 0 {
		t.Error("short payload should not be relayed")
	}
}

func TestHandleRelay_UnknownSender(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))

	// Sender not in addrMap
	payload := make([]byte, 4+4+3)
	copy(payload[0:4], net.IPv4(10, 10, 0, 2).To4())
	copy(payload[4:8], net.IPv4(10, 10, 0, 3).To4())

	r.handleRelay(payload, &net.UDPAddr{IP: net.IPv4(99, 99, 99, 99), Port: 1000})

	if r.totalPacketsRelay.Load() != 0 {
		t.Error("unknown sender should not relay")
	}
}

func TestHandleRelay_TokenValidation_OldFormat(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))
	conn, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	r.conn = conn
	defer conn.Close()

	senderAddr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1000}
	receiverAddr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 5), Port: 2000}

	sender := &Client{
		Username:      "sender",
		VirtualIP:     net.IPv4(10, 10, 0, 2),
		PublicAddr:    senderAddr,
		auth:          authNone,
		clientVersion: protocol.MinTokenVersion,
	}
	sender.SetLastSeen(time.Now())
	sender.GenerateSessionToken()

	receiver := &Client{
		Username:   "receiver",
		VirtualIP:  net.IPv4(10, 10, 0, 3),
		PublicAddr: receiverAddr,
		auth:       authNone,
	}
	receiver.SetLastSeen(time.Now())

	r.mu.Lock()
	r.clients[ipKey(sender.VirtualIP)] = sender
	r.clients[ipKey(receiver.VirtualIP)] = receiver
	r.addrMap[addrToRateKey(senderAddr)] = sender
	r.addrMap[addrToRateKey(receiverAddr)] = receiver
	r.mu.Unlock()

	// Old format (no formatVer): srcIP(4) + dstIP(4) + flags(1) + token(16) + data
	payload := make([]byte, 4+4+1+16+5)
	copy(payload[0:4], sender.VirtualIP.To4())
	copy(payload[4:8], receiver.VirtualIP.To4())
	payload[8] = protocol.DataFlagHasToken
	copy(payload[9:25], sender.SessionToken[:])
	copy(payload[25:], []byte{0x01, 0x02, 0x03, 0x04, 0x05})

	r.handleRelay(payload, senderAddr)

	if r.totalPacketsRelay.Load() != 1 {
		t.Errorf("valid old-format token: totalPacketsRelay = %d, want 1", r.totalPacketsRelay.Load())
	}
}

func TestHandleRelay_TokenValidation_NewFormat(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))
	conn, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	r.conn = conn
	defer conn.Close()

	senderAddr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1000}
	receiverAddr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 5), Port: 2000}

	sender := &Client{
		Username:      "sender",
		VirtualIP:     net.IPv4(10, 10, 0, 2),
		PublicAddr:    senderAddr,
		auth:          authNone,
		clientVersion: protocol.MinTokenVersion,
	}
	sender.SetLastSeen(time.Now())
	sender.GenerateSessionToken()

	receiver := &Client{
		Username:   "receiver",
		VirtualIP:  net.IPv4(10, 10, 0, 3),
		PublicAddr: receiverAddr,
		auth:       authNone,
	}
	receiver.SetLastSeen(time.Now())

	r.mu.Lock()
	r.clients[ipKey(sender.VirtualIP)] = sender
	r.clients[ipKey(receiver.VirtualIP)] = receiver
	r.addrMap[addrToRateKey(senderAddr)] = sender
	r.addrMap[addrToRateKey(receiverAddr)] = receiver
	r.mu.Unlock()

	// New format (v1.8+): srcIP(4) + dstIP(4) + formatVer(1) + flags(1) + token(16) + data
	payload := make([]byte, 4+4+1+1+16+5)
	copy(payload[0:4], sender.VirtualIP.To4())
	copy(payload[4:8], receiver.VirtualIP.To4())
	payload[8] = protocol.DataFormatVersion
	payload[9] = protocol.DataFlagHasToken
	copy(payload[10:26], sender.SessionToken[:])
	copy(payload[26:], []byte{0x01, 0x02, 0x03, 0x04, 0x05})

	r.handleRelay(payload, senderAddr)

	if r.totalPacketsRelay.Load() != 1 {
		t.Errorf("valid new-format token: totalPacketsRelay = %d, want 1", r.totalPacketsRelay.Load())
	}
}

func TestHandleRelay_TokenValidation_WrongToken(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))
	conn, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	r.conn = conn
	defer conn.Close()

	senderAddr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1000}
	receiverAddr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 5), Port: 2000}

	sender := &Client{
		Username:      "sender",
		VirtualIP:     net.IPv4(10, 10, 0, 2),
		PublicAddr:    senderAddr,
		auth:          authNone,
		clientVersion: protocol.MinTokenVersion,
	}
	sender.SetLastSeen(time.Now())
	sender.GenerateSessionToken()

	receiver := &Client{
		Username:   "receiver",
		VirtualIP:  net.IPv4(10, 10, 0, 3),
		PublicAddr: receiverAddr,
		auth:       authNone,
	}
	receiver.SetLastSeen(time.Now())

	r.mu.Lock()
	r.clients[ipKey(sender.VirtualIP)] = sender
	r.clients[ipKey(receiver.VirtualIP)] = receiver
	r.addrMap[addrToRateKey(senderAddr)] = sender
	r.addrMap[addrToRateKey(receiverAddr)] = receiver
	r.mu.Unlock()

	// New format with WRONG token
	payload := make([]byte, 4+4+1+1+16+5)
	copy(payload[0:4], sender.VirtualIP.To4())
	copy(payload[4:8], receiver.VirtualIP.To4())
	payload[8] = protocol.DataFormatVersion
	payload[9] = protocol.DataFlagHasToken
	// Explicitly zero the token field so it won't match sender.SessionToken
	clear(payload[10:26])
	copy(payload[26:], []byte{0x01, 0x02, 0x03, 0x04, 0x05})

	r.handleRelay(payload, senderAddr)

	if r.totalPacketsRelay.Load() != 0 {
		t.Errorf("wrong token should be rejected: totalPacketsRelay = %d, want 0", r.totalPacketsRelay.Load())
	}
}

// ── handleHolePunch Tests ──────────────────────────────────────

func TestHandleHolePunch_Valid(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))
	conn, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	r.conn = conn
	defer conn.Close()

	srcAddr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1000}
	dstAddr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 5), Port: 2000}

	src := &Client{
		Username:  "src",
		VirtualIP: net.IPv4(10, 10, 0, 2),
		PublicAddr: srcAddr,
		auth:      authNone,
	}
	src.SetLastSeen(time.Now())
	dst := &Client{
		Username:  "dst",
		VirtualIP: net.IPv4(10, 10, 0, 3),
		PublicAddr: dstAddr,
		auth:      authNone,
	}
	dst.SetLastSeen(time.Now())

	r.mu.Lock()
	r.clients[ipKey(src.VirtualIP)] = src
	r.clients[ipKey(dst.VirtualIP)] = dst
	r.addrMap[addrToRateKey(srcAddr)] = src
	r.addrMap[addrToRateKey(dstAddr)] = dst
	r.mu.Unlock()

	// Build hole punch payload: dstIP(4)
	payload := make([]byte, 4)
	copy(payload, dst.VirtualIP.To4())

	r.handleHolePunch(payload, srcAddr)

	// Should have sent to dst
	// (can't easily verify packet content without reading from dstAddr)
}

func TestHandleHolePunch_ShortPayload(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))

	// Payload too short (< 4 bytes)
	r.handleHolePunch([]byte{0x01}, &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1000})
	// Should not panic
}

func TestHandleHolePunch_UnknownSource(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))

	dstIP := net.IPv4(10, 10, 0, 3).To4()
	payload := make([]byte, 4)
	copy(payload, dstIP)

	// Source not in addrMap
	r.handleHolePunch(payload, &net.UDPAddr{IP: net.IPv4(99, 99, 99, 99), Port: 1000})
	// Should not panic
}

// ── handlePong Tests ───────────────────────────────────────────

func TestHandlePong_Valid(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))

	addr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345}
	c := &Client{
		Username:  "player1",
		VirtualIP: net.IPv4(10, 10, 0, 2),
		PublicAddr: addr,
		auth:      authNone,
	}
	c.SetLastSeen(time.Now())

	r.mu.Lock()
	r.clients[ipKey(c.VirtualIP)] = c
	r.addrMap[addrToRateKey(addr)] = c
	r.mu.Unlock()

	// Build pong payload with recent timestamp
	ping := &protocol.PingPayload{Timestamp: time.Now().Add(-50 * time.Millisecond).UnixNano()}
	r.handlePong(ping.Marshal(), addr)

	r.mu.RLock()
	updated := r.addrMap[addrToRateKey(addr)]
	r.mu.RUnlock()

	if updated.RTT <= 0 {
		t.Error("RTT should be positive after pong")
	}
}

func TestHandlePong_InvalidTimestamp(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))

	addr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345}
	c := &Client{
		Username:  "player1",
		VirtualIP: net.IPv4(10, 10, 0, 2),
		PublicAddr: addr,
		auth:      authNone,
	}
	c.SetLastSeen(time.Now())

	r.mu.Lock()
	r.clients[ipKey(c.VirtualIP)] = c
	r.addrMap[addrToRateKey(addr)] = c
	r.mu.Unlock()

	// Future timestamp (invalid)
	ping := &protocol.PingPayload{Timestamp: time.Now().Add(1 * time.Hour).UnixNano()}
	r.handlePong(ping.Marshal(), addr)

	r.mu.RLock()
	updated := r.addrMap[addrToRateKey(addr)]
	r.mu.RUnlock()

	if updated.RTT != 0 {
		t.Error("RTT should be 0 for invalid timestamp")
	}
}

func TestHandlePong_UnknownClient(t *testing.T) {
	r := newTestRoom("10.10.0.0/24", net.IPv4(10, 10, 0, 1))

	addr := &net.UDPAddr{IP: net.IPv4(99, 99, 99, 99), Port: 12345}
	ping := &protocol.PingPayload{Timestamp: time.Now().UnixNano()}

	// Should not panic
	r.handlePong(ping.Marshal(), addr)
}

// ── BandwidthLimiter Tests ─────────────────────────────────────

func TestBandwidthLimiter_Allow(t *testing.T) {
	bl := NewBandwidthLimiter(1024) // 1KB/s

	addr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345}

	// Should allow small packets
	if !bl.Allow(addr, 100) {
		t.Error("should allow small packet")
	}
}

func TestBandwidthLimiter_Remove(t *testing.T) {
	bl := NewBandwidthLimiter(1024)

	addr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345}

	// Create bucket
	bl.Allow(addr, 100)

	// Remove
	bl.Remove(addr)

	// Should create new bucket on next access
	if !bl.Allow(addr, 100) {
		t.Error("should allow after remove")
	}
}

func TestBandwidthLimiter_NilLimiter(t *testing.T) {
	var bl *BandwidthLimiter

	addr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345}

	// Should always allow
	if !bl.Allow(addr, 100) {
		t.Error("nil limiter should always allow")
	}
}

func TestBandwidthLimiter_Disabled(t *testing.T) {
	bl := NewBandwidthLimiter(0) // disabled

	addr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345}

	if !bl.Allow(addr, 100) {
		t.Error("disabled limiter should always allow")
	}
}

// ── MetricsTimeSeries Tests ────────────────────────────────────

func TestMetricsTimeSeries_AppendAndSnapshot(t *testing.T) {
	ts := NewMetricsTimeSeries()

	// Empty snapshot
	if ts.Snapshot() != nil {
		t.Error("expected nil for empty series")
	}

	// Append samples
	for i := 0; i < 5; i++ {
		ts.Append(MetricsSample{
			Timestamp: int64(i),
			Players:   i * 10,
		})
	}

	samples := ts.Snapshot()
	if len(samples) != 5 {
		t.Errorf("got %d samples, want 5", len(samples))
	}

	// Should be in chronological order
	for i, s := range samples {
		if s.Timestamp != int64(i) {
			t.Errorf("sample[%d].Timestamp: got %d, want %d", i, s.Timestamp, i)
		}
	}
}

func TestMetricsTimeSeries_Wraparound(t *testing.T) {
	ts := NewMetricsTimeSeries()

	// Fill beyond metricsSlots (60)
	for i := 0; i < 70; i++ {
		ts.Append(MetricsSample{
			Timestamp: int64(i),
			Players:   i,
		})
	}

	samples := ts.Snapshot()
	if len(samples) != metricsSlots {
		t.Errorf("got %d samples, want %d", len(samples), metricsSlots)
	}

	// Should contain the most recent 60 samples
	if samples[0].Timestamp != 10 { // 70 - 60 = 10
		t.Errorf("first sample Timestamp: got %d, want 10", samples[0].Timestamp)
	}
}

// ── Bandwidth Limiter Cleanup ────────────────────────────────

func TestBandwidthLimiter_Cleanup(t *testing.T) {
	bl := NewBandwidthLimiter(1000)
	addr1 := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 100}
	addr2 := &net.UDPAddr{IP: net.IPv4(5, 6, 7, 8), Port: 200}

	// Create two buckets
	bl.Allow(addr1, 10)
	bl.Allow(addr2, 10)

	// Both should exist
	if !bl.Enabled() {
		t.Error("expected limiter to be enabled")
	}

	// Cleanup with 0 duration should remove all
	bl.Cleanup(0)

	// After cleanup, new Allow calls create fresh buckets
	bl.Allow(addr1, 10)
}

func TestBandwidthLimiter_CleanupDisabled(t *testing.T) {
	bl := NewBandwidthLimiter(0) // disabled
	// Cleanup should not panic
	bl.Cleanup(time.Minute)
}

func TestSubUint64(t *testing.T) {
	tests := []struct {
		a, b uint64
		want uint64
	}{
		{10, 5, 5},
		{5, 10, 0}, // clamp to 0
		{0, 0, 0},
		{^uint64(0), 1, ^uint64(0) - 1},
	}

	for _, tt := range tests {
		got := subUint64(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("subUint64(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}
