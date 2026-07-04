package client

import (
	"github.com/holipay/gametunnel/internal/crypto"
	"github.com/holipay/gametunnel/internal/netkey"
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/holipay/gametunnel/internal/protocol"
)

// ipKeyPtr is a test helper that returns a pointer to a [16]byte IP key.
func ipKeyPtr(ip net.IP) *[16]byte {
	k := netkey.IPKey(ip)
	return &k
}

// ── Mock TunDevice ─────────────────────────────────────────────

type mockTunDevice struct {
	readData []byte
	readErr  error
	writeBuf []byte
	writeErr error
	closed   bool
}

func (m *mockTunDevice) Read(buf []byte) (int, error) {
	if m.readErr != nil {
		return 0, m.readErr
	}
	n := copy(buf, m.readData)
	return n, nil
}

func (m *mockTunDevice) Write(data []byte) (int, error) {
	if m.writeErr != nil {
		return 0, m.writeErr
	}
	m.writeBuf = append(m.writeBuf, data...)
	return len(data), nil
}

func (m *mockTunDevice) Close() error {
	m.closed = true
	return nil
}

func (m *mockTunDevice) Name() string {
	return "mocktun0"
}

func (m *mockTunDevice) MTU() int {
	return 1500
}

func (m *mockTunDevice) ReconfigureRoutes() {}

// ── Test Helpers ───────────────────────────────────────────────

// newTestTunnel creates a Tunnel with a real UDP conn for testing.
// Returns the tunnel and a "server" UDP listener.
// Both sockets are bound to 127.0.0.1 for cross-platform reliability.
func newTestTunnel(t *testing.T) (*Tunnel, *net.UDPConn) {
	t.Helper()

	tunnelConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("failed to create tunnel conn: %v", err)
	}
	t.Cleanup(func() { tunnelConn.Close() })

	serverConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("failed to create server conn: %v", err)
	}
	t.Cleanup(func() { serverConn.Close() })

	cfg := &Config{PlayerName: "test", RoomID: "test", RoomPassword: ""}
	tunnel := New(cfg)
	tunnel.conn = tunnelConn
	tunnel.serverAddr.Store(serverConn.LocalAddr().(*net.UDPAddr))

	// ── 启动 sendLoop，让 sendCh 中的数据能实际写入 UDP ──
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go tunnel.sendLoop(ctx, tunnel.conn)

	return tunnel, serverConn
}

// readUDPWithTimeout reads one packet from conn with a timeout.
// Returns nil if no packet arrives within the timeout.
func readUDPWithTimeout(conn *net.UDPConn, timeout time.Duration) []byte {
	conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 65535)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		return nil
	}
	return buf[:n]
}

// ── 1. ipKey Tests ────────────────────────────────────────────

func TestIpKey_NormalIPv4(t *testing.T) {
	ip := net.IPv4(192, 168, 1, 1).To4()
	key := netkey.IPKey(ip)
	// IPv4 is mapped to v4-in-v6 format
	expected := [16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 192, 168, 1, 1}
	if key != expected {
		t.Errorf("expected %v, got %v", expected, key)
	}
}

func TestIpKey_IPv4Mapped(t *testing.T) {
	// 16-byte IPv4-mapped address: ::ffff:192.168.1.1
	ip16 := net.IP{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 192, 168, 1, 1}
	key := netkey.IPKey(ip16)
	expected := [16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 192, 168, 1, 1}
	if key != expected {
		t.Errorf("expected %v, got %v", expected, key)
	}
}

func TestIpKey_Consistency(t *testing.T) {
	// Both 4-byte and 16-byte representations of the same IP must produce the same key
	ip4 := net.IPv4(10, 0, 0, 1).To4()
	ip16 := net.IP{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 10, 0, 0, 1}
	if netkey.IPKey(ip4) != netkey.IPKey(ip16) {
		t.Error("4-byte and 16-byte IPv4-mapped should produce the same key")
	}
}

// ── 2. routePacket Tests ───────────────────────────────────────

func TestRoutePacket_Broadcast(t *testing.T) {
	tunnel, serverConn := newTestTunnel(t)

	tunnel.session.serverIP = net.IPv4(10, 0, 0, 1).To4()
	tunnel.session.serverIPKey.Store(ipKeyPtr(tunnel.session.serverIP))
	tunnel.session.cachedSubnet.Store(&net.IPNet{
		IP:   net.IPv4(10, 0, 0, 0).To4(),
		Mask: net.CIDRMask(24, 32),
	})

	pkt := []byte{0x45, 0, 0, 20, 0, 0, 0, 0, 64, 17, 0, 0, 10, 0, 0, 2, 255, 255, 255, 255}
	var srcIP, dstIP [4]byte
	copy(srcIP[:], net.IPv4(10, 0, 0, 2).To4())
	copy(dstIP[:], net.IPv4(255, 255, 255, 255).To4())

	go tunnel.routePacket(pkt, srcIP, dstIP)

	data := readUDPWithTimeout(serverConn, 2*time.Second)
	if data == nil {
		t.Fatal("expected packet on server conn for broadcast, got none")
	}

	msg, err := protocol.DecodeChecked(data)
	if err != nil {
		t.Fatalf("failed to decode packet: %v", err)
	}
	if msg.Type != protocol.TypeData {
		t.Errorf("expected TypeData (0x%02x), got 0x%02x", protocol.TypeData, msg.Type)
	}

	dp, err := protocol.UnmarshalData(msg.Payload)
	if err != nil {
		t.Fatalf("failed to unmarshal DataPayload: %v", err)
	}
	if !dp.DstIP.Equal(net.IPv4bcast) {
		t.Errorf("expected DstIP 255.255.255.255, got %s", dp.DstIP)
	}
}

func TestRoutePacket_ServerIP(t *testing.T) {
	tunnel, serverConn := newTestTunnel(t)

	serverIP := net.IPv4(10, 0, 0, 1).To4()
	tunnel.session.serverIP = serverIP
	tunnel.session.serverIPKey.Store(ipKeyPtr(serverIP))

	pkt := []byte{0x45, 0, 0, 20}
	var srcIP, dstIP [4]byte
	copy(srcIP[:], net.IPv4(10, 0, 0, 2).To4())
	copy(dstIP[:], serverIP)

	go tunnel.routePacket(pkt, srcIP, dstIP)

	data := readUDPWithTimeout(serverConn, 2*time.Second)
	if data == nil {
		t.Fatal("expected packet on server conn for serverIP dest, got none")
	}

	msg, err := protocol.DecodeChecked(data)
	if err != nil {
		t.Fatalf("failed to decode packet: %v", err)
	}
	if msg.Type != protocol.TypeData {
		t.Errorf("expected TypeData (0x%02x), got 0x%02x", protocol.TypeData, msg.Type)
	}

	dp, err := protocol.UnmarshalData(msg.Payload)
	if err != nil {
		t.Fatalf("failed to unmarshal DataPayload: %v", err)
	}
	if !dp.DstIP.Equal(serverIP) {
		t.Errorf("expected DstIP %s, got %s", serverIP, dp.DstIP)
	}
}

func TestRoutePacket_PeerP2P(t *testing.T) {
	tunnel, serverConn := newTestTunnel(t)

	// Create a separate listener to act as the peer endpoint
	peerConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("failed to create peer conn: %v", err)
	}
	t.Cleanup(func() { peerConn.Close() })

	serverIP := net.IPv4(10, 0, 0, 1).To4()
	tunnel.session.serverIP = serverIP
	tunnel.session.serverIPKey.Store(ipKeyPtr(serverIP))

	peerIP := net.IPv4(10, 0, 0, 3).To4()
	peerAddr := peerConn.LocalAddr().(*net.UDPAddr)
	peer := &Peer{
		VirtualIP:  peerIP,
		Username:   "peer1",
	}
	peer.PublicAddr.Store(peerAddr)
	peer.DirectReach.Store(true)
	tunnel.peers = map[[16]byte]*Peer{
		netkey.IPKey(peerIP): peer,
	}

	pkt := []byte{0x45, 0, 0, 20}
	srcIP := net.IPv4(10, 0, 0, 2).To4()
	var srcIP4, dstIP4 [4]byte
	copy(srcIP4[:], srcIP)
	copy(dstIP4[:], peerIP)

	go tunnel.routePacket(pkt, srcIP4, dstIP4)

	// Peer should receive the packet
	peerData := readUDPWithTimeout(peerConn, 2*time.Second)
	if peerData == nil {
		t.Fatal("expected packet on peer conn for P2P, got none")
	}

	// Server should NOT receive the packet
	serverData := readUDPWithTimeout(serverConn, 200*time.Millisecond)
	if serverData != nil {
		t.Error("expected no packet on server conn for P2P, but got one")
	}

	// Verify packet content
	msg, err := protocol.DecodeChecked(peerData)
	if err != nil {
		t.Fatalf("failed to decode peer packet: %v", err)
	}
	if msg.Type != protocol.TypeData {
		t.Errorf("expected TypeData, got 0x%02x", msg.Type)
	}

	dp, err := protocol.UnmarshalData(msg.Payload)
	if err != nil {
		t.Fatalf("failed to unmarshal DataPayload: %v", err)
	}
	if !dp.SrcIP.Equal(srcIP) {
		t.Errorf("expected SrcIP %s, got %s", srcIP, dp.SrcIP)
	}
	if !dp.DstIP.Equal(peerIP) {
		t.Errorf("expected DstIP %s, got %s", peerIP, dp.DstIP)
	}
	if !bytes.Equal(dp.Data, pkt) {
		t.Errorf("expected Data %v, got %v", pkt, dp.Data)
	}
}

func TestRoutePacket_PeerNilAddr(t *testing.T) {
	tunnel, serverConn := newTestTunnel(t)

	serverIP := net.IPv4(10, 0, 0, 1).To4()
	tunnel.session.serverIP = serverIP
	tunnel.session.serverIPKey.Store(ipKeyPtr(serverIP))

	// Peer exists but has no PublicAddr (hole punch not yet completed)
	peerIP := net.IPv4(10, 0, 0, 3).To4()
	peer := &Peer{
		VirtualIP:  peerIP,
		Username:   "peer1",
	}
	tunnel.peers = map[[16]byte]*Peer{
		netkey.IPKey(peerIP): peer,
	}

	pkt := []byte{0x45, 0, 0, 20}
	srcIP := net.IPv4(10, 0, 0, 2).To4()
	var srcIP4, dstIP4 [4]byte
	copy(srcIP4[:], srcIP)
	copy(dstIP4[:], peerIP)

	go tunnel.routePacket(pkt, srcIP4, dstIP4)

	// Should fall back to server relay
	data := readUDPWithTimeout(serverConn, 2*time.Second)
	if data == nil {
		t.Fatal("expected packet on server conn (fallback for nil addr peer), got none")
	}

	msg, err := protocol.DecodeChecked(data)
	if err != nil {
		t.Fatalf("failed to decode packet: %v", err)
	}
	if msg.Type != protocol.TypeData {
		t.Errorf("expected TypeData, got 0x%02x", msg.Type)
	}
}

func TestRoutePacket_UnknownIP(t *testing.T) {
	tunnel, serverConn := newTestTunnel(t)

	serverIP := net.IPv4(10, 0, 0, 1).To4()
	tunnel.session.serverIP = serverIP
	tunnel.session.serverIPKey.Store(ipKeyPtr(serverIP))

	pkt := []byte{0x45, 0, 0, 20}
	srcIP := net.IPv4(10, 0, 0, 2).To4()
	dstIP := net.IPv4(10, 0, 0, 99).To4() // unknown IP, not in peers
	var srcIP4, dstIP4 [4]byte
	copy(srcIP4[:], srcIP)
	copy(dstIP4[:], dstIP)

	go tunnel.routePacket(pkt, srcIP4, dstIP4)

	data := readUDPWithTimeout(serverConn, 2*time.Second)
	if data == nil {
		t.Fatal("expected packet on server conn (fallback for unknown IP), got none")
	}

	msg, err := protocol.DecodeChecked(data)
	if err != nil {
		t.Fatalf("failed to decode packet: %v", err)
	}
	if msg.Type != protocol.TypeData {
		t.Errorf("expected TypeData, got 0x%02x", msg.Type)
	}

	dp, err := protocol.UnmarshalData(msg.Payload)
	if err != nil {
		t.Fatalf("failed to unmarshal DataPayload: %v", err)
	}
	if !dp.DstIP.Equal(dstIP) {
		t.Errorf("expected DstIP %s, got %s", dstIP, dp.DstIP)
	}
}

// ── 3. handlePeerInfo Tests ────────────────────────────────────

func TestHandlePeerInfo_NewPlayer(t *testing.T) {
	tunnel, _ := newTestTunnel(t)

	// Create a listener for the peer's PublicAddr
	peerConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("failed to create peer conn: %v", err)
	}
	t.Cleanup(func() { peerConn.Close() })

	peerIP := net.IPv4(10, 0, 0, 5).To4()
	peerAddr := peerConn.LocalAddr().(*net.UDPAddr)

	payload := &protocol.PeerInfoPayload{
		Peers: []protocol.PeerInfoEntry{
			{
				VirtualIP:  peerIP,
				PublicAddr: peerAddr,
				Username:   "newplayer",
			},
		},
	}

	tunnel.handlePeerInfo(context.Background(), payload.Marshal())

	tunnel.mu.RLock()
	peer, ok := tunnel.peers[netkey.IPKey(peerIP)]
	tunnel.mu.RUnlock()

	if !ok {
		t.Fatal("expected peer to be added to peers map")
	}
	if peer.Username != "newplayer" {
		t.Errorf("expected username 'newplayer', got '%s'", peer.Username)
	}
	if !peer.VirtualIP.Equal(peerIP) {
		t.Errorf("expected VirtualIP %s, got %s", peerIP, peer.VirtualIP)
	}
	if peer.PublicAddr.Load().String() != peerAddr.String() {
		t.Errorf("expected PublicAddr %s, got %s", peerAddr, peer.PublicAddr.Load())
	}

	// Allow startHolePunch goroutine to start
	time.Sleep(100 * time.Millisecond)
}

func TestHandlePeerInfo_UpdatePlayer(t *testing.T) {
	tunnel, _ := newTestTunnel(t)

	peerIP := net.IPv4(10, 0, 0, 5).To4()
	oldAddr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1000}
	newAddr := &net.UDPAddr{IP: net.IPv4(5, 6, 7, 8), Port: 2000}

	// Pre-populate peers
	existingPeer := &Peer{
		VirtualIP:  peerIP,
		Username:   "player1",
	}
	existingPeer.PublicAddr.Store(oldAddr)
	tunnel.peers = map[[16]byte]*Peer{
		netkey.IPKey(peerIP): existingPeer,
	}

	payload := &protocol.PeerInfoPayload{
		Peers: []protocol.PeerInfoEntry{
			{
				VirtualIP:  peerIP,
				PublicAddr: newAddr,
				Username:   "player1",
			},
		},
	}

	tunnel.handlePeerInfo(context.Background(), payload.Marshal())

	tunnel.mu.RLock()
	peer, ok := tunnel.peers[netkey.IPKey(peerIP)]
	tunnel.mu.RUnlock()

	if !ok {
		t.Fatal("expected peer to still be in peers map")
	}
	if peer.PublicAddr.Load().String() != newAddr.String() {
		t.Errorf("expected PublicAddr %s, got %s", newAddr, peer.PublicAddr.Load())
	}
	if peer.Username != "player1" {
		t.Errorf("expected Username 'player1', got '%s'", peer.Username)
	}
}

func TestHandlePeerInfo_PlayerLeaving(t *testing.T) {
	tunnel, _ := newTestTunnel(t)

	peer1IP := net.IPv4(10, 0, 0, 5).To4()
	peer2IP := net.IPv4(10, 0, 0, 6).To4()

	// Pre-populate with two peers
	player1 := &Peer{
		VirtualIP:  peer1IP,
		Username:   "player1",
	}
	player1.PublicAddr.Store(&net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1000})
	player2 := &Peer{
		VirtualIP:  peer2IP,
		Username:   "player2",
	}
	player2.PublicAddr.Store(&net.UDPAddr{IP: net.IPv4(5, 6, 7, 8), Port: 2000})
	tunnel.peers = map[[16]byte]*Peer{
		netkey.IPKey(peer1IP): player1,
		netkey.IPKey(peer2IP): player2,
	}

	// Server sends updated list containing only peer2; peer1 has left
	payload := &protocol.PeerInfoPayload{
		Peers: []protocol.PeerInfoEntry{
			{
				VirtualIP:  peer2IP,
				PublicAddr: &net.UDPAddr{IP: net.IPv4(5, 6, 7, 8), Port: 2000},
				Username:   "player2",
			},
		},
	}

	tunnel.handlePeerInfo(context.Background(), payload.Marshal())

	tunnel.mu.RLock()
	_, hasPeer1 := tunnel.peers[netkey.IPKey(peer1IP)]
	_, hasPeer2 := tunnel.peers[netkey.IPKey(peer2IP)]
	tunnel.mu.RUnlock()

	if hasPeer1 {
		t.Error("expected peer1 to be removed from peers map")
	}
	if !hasPeer2 {
		t.Error("expected peer2 to still be in peers map")
	}
}

func TestHandlePeerInfo_EmptyList(t *testing.T) {
	tunnel, _ := newTestTunnel(t)

	peerIP := net.IPv4(10, 0, 0, 5).To4()
	player := &Peer{
		VirtualIP:  peerIP,
		Username:   "player1",
	}
	player.PublicAddr.Store(&net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1000})
	tunnel.peers = map[[16]byte]*Peer{
		netkey.IPKey(peerIP): player,
	}

	// Server sends empty peer list
	payload := &protocol.PeerInfoPayload{Peers: []protocol.PeerInfoEntry{}}
	tunnel.handlePeerInfo(context.Background(), payload.Marshal())

	tunnel.mu.RLock()
	count := len(tunnel.peers)
	tunnel.mu.RUnlock()

	if count != 0 {
		t.Errorf("expected 0 peers after empty list, got %d", count)
	}
}

// ── 4. config.go Tests ─────────────────────────────────────────

func TestLoadINI(t *testing.T) {
	tmpDir := t.TempDir()
	iniPath := filepath.Join(tmpDir, "config.ini")

	content := "server=1.2.3.4:4700\nname=TestPlayer\nroom=myroom\npassword=secret123\n"
	if err := os.WriteFile(iniPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write INI file: %v", err)
	}

	cfg := &Config{PlayerName: "default", RoomID: "default"}
	ok := loadINI(iniPath, cfg)
	if !ok {
		t.Fatal("expected loadINI to return true")
	}

	if cfg.ServerAddr != "1.2.3.4:4700" {
		t.Errorf("expected ServerAddr '1.2.3.4:4700', got '%s'", cfg.ServerAddr)
	}
	if cfg.PlayerName != "TestPlayer" {
		t.Errorf("expected PlayerName 'TestPlayer', got '%s'", cfg.PlayerName)
	}
	if cfg.RoomID != "myroom" {
		t.Errorf("expected RoomID 'myroom', got '%s'", cfg.RoomID)
	}
	if cfg.RoomPassword != "secret123" {
		t.Errorf("expected RoomPassword 'secret123', got '%s'", cfg.RoomPassword)
	}
}

func TestLoadINI_NotFound(t *testing.T) {
	cfg := &Config{PlayerName: "default", RoomID: "default"}
	ok := loadINI("/nonexistent/path/config.ini", cfg)
	if ok {
		t.Error("expected loadINI to return false for nonexistent file")
	}
	// cfg should remain unchanged
	if cfg.ServerAddr != "" {
		t.Errorf("expected ServerAddr '', got '%s'", cfg.ServerAddr)
	}
}

func TestLoadINI_CommentsAndBlanks(t *testing.T) {
	tmpDir := t.TempDir()
	iniPath := filepath.Join(tmpDir, "config.ini")

	content := "# This is a comment\nserver=1.2.3.4:4700\n\n   \n# Another comment\nname=TestPlayer\n"
	if err := os.WriteFile(iniPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write INI file: %v", err)
	}

	cfg := &Config{PlayerName: "default", RoomID: "default"}
	ok := loadINI(iniPath, cfg)
	if !ok {
		t.Fatal("expected loadINI to return true")
	}

	if cfg.ServerAddr != "1.2.3.4:4700" {
		t.Errorf("expected ServerAddr '1.2.3.4:4700', got '%s'", cfg.ServerAddr)
	}
	if cfg.PlayerName != "TestPlayer" {
		t.Errorf("expected PlayerName 'TestPlayer', got '%s'", cfg.PlayerName)
	}
}

func TestLoadINI_EmptyValuesPreserveDefaults(t *testing.T) {
	tmpDir := t.TempDir()
	iniPath := filepath.Join(tmpDir, "config.ini")

	content := "server=1.2.3.4:4700\nname=\nroom=\n"
	if err := os.WriteFile(iniPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write INI file: %v", err)
	}

	cfg := &Config{PlayerName: "defaultName", RoomID: "defaultRoom"}
	ok := loadINI(iniPath, cfg)
	if !ok {
		t.Fatal("expected loadINI to return true")
	}

	// Empty values should not overwrite defaults
	if cfg.PlayerName != "defaultName" {
		t.Errorf("expected PlayerName 'defaultName', got '%s'", cfg.PlayerName)
	}
	if cfg.RoomID != "defaultRoom" {
		t.Errorf("expected RoomID 'defaultRoom', got '%s'", cfg.RoomID)
	}
}

func TestLoadINI_SeparateServerPort(t *testing.T) {
	tmpDir := t.TempDir()
	iniPath := filepath.Join(tmpDir, "config.ini")

	// Separated format: server=host, port=port
	content := "server=192.168.1.1\nport=5000\nname=Player1\nroom=test\npassword=abc\n"
	if err := os.WriteFile(iniPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write INI file: %v", err)
	}

	cfg := &Config{PlayerName: "default", RoomID: "default"}
	ok := loadINI(iniPath, cfg)
	if !ok {
		t.Fatal("expected loadINI to return true")
	}

	if cfg.ServerAddr != "192.168.1.1:5000" {
		t.Errorf("expected ServerAddr '192.168.1.1:5000', got '%s'", cfg.ServerAddr)
	}
	if cfg.PlayerName != "Player1" {
		t.Errorf("expected PlayerName 'Player1', got '%s'", cfg.PlayerName)
	}
	if cfg.RoomID != "test" {
		t.Errorf("expected RoomID 'test', got '%s'", cfg.RoomID)
	}
	if cfg.RoomPassword != "abc" {
		t.Errorf("expected RoomPassword 'abc', got '%s'", cfg.RoomPassword)
	}
}

func TestLoadINI_SeparateIPv6Port(t *testing.T) {
	tmpDir := t.TempDir()
	iniPath := filepath.Join(tmpDir, "config.ini")

	// IPv6 separated format: server=ipv6addr, port=port
	content := "server=240d:c000:f07f:8e00:3ab0:2dee:7c06:0\nport=4700\nname=ipv6player\n"
	if err := os.WriteFile(iniPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write INI file: %v", err)
	}

	cfg := &Config{PlayerName: "default", RoomID: "default"}
	ok := loadINI(iniPath, cfg)
	if !ok {
		t.Fatal("expected loadINI to return true")
	}

	expected := "[240d:c000:f07f:8e00:3ab0:2dee:7c06:0]:4700"
	if cfg.ServerAddr != expected {
		t.Errorf("expected ServerAddr '%s', got '%s'", expected, cfg.ServerAddr)
	}
	if cfg.PlayerName != "ipv6player" {
		t.Errorf("expected PlayerName 'ipv6player', got '%s'", cfg.PlayerName)
	}
}

func TestLoadINI_ServerWithPortIgnoresSeparatePort(t *testing.T) {
	tmpDir := t.TempDir()
	iniPath := filepath.Join(tmpDir, "config.ini")

	// When server already has port, separate port= should be ignored
	content := "server=1.2.3.4:8000\nport=9000\n"
	if err := os.WriteFile(iniPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write INI file: %v", err)
	}

	cfg := &Config{PlayerName: "default", RoomID: "default"}
	ok := loadINI(iniPath, cfg)
	if !ok {
		t.Fatal("expected loadINI to return true")
	}

	if cfg.ServerAddr != "1.2.3.4:8000" {
		t.Errorf("expected ServerAddr '1.2.3.4:8000', got '%s'", cfg.ServerAddr)
	}
}

func TestLoadJSON(t *testing.T) {
	tmpDir := t.TempDir()
	jsonPath := filepath.Join(tmpDir, "config.json")

	content := `{
		"server_addr": "5.6.7.8:4700",
		"player_name": "JSONPlayer",
		"room_id": "jsonroom",
		"room_password": "jsonpass"
	}`
	if err := os.WriteFile(jsonPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write JSON file: %v", err)
	}

	cfg := &Config{PlayerName: "default", RoomID: "default"}
	loadJSON(jsonPath, cfg)

	if cfg.ServerAddr != "5.6.7.8:4700" {
		t.Errorf("expected ServerAddr '5.6.7.8:4700', got '%s'", cfg.ServerAddr)
	}
	if cfg.PlayerName != "JSONPlayer" {
		t.Errorf("expected PlayerName 'JSONPlayer', got '%s'", cfg.PlayerName)
	}
	if cfg.RoomID != "jsonroom" {
		t.Errorf("expected RoomID 'jsonroom', got '%s'", cfg.RoomID)
	}
	if cfg.RoomPassword != "jsonpass" {
		t.Errorf("expected RoomPassword 'jsonpass', got '%s'", cfg.RoomPassword)
	}
}

func TestLoadJSON_NotFound(t *testing.T) {
	cfg := &Config{PlayerName: "default", RoomID: "default"}
	loadJSON("/nonexistent/path/config.json", cfg)
	// Should not panic; cfg should remain unchanged
	if cfg.PlayerName != "default" {
		t.Errorf("expected PlayerName 'default', got '%s'", cfg.PlayerName)
	}
	if cfg.RoomID != "default" {
		t.Errorf("expected RoomID 'default', got '%s'", cfg.RoomID)
	}
}

func TestLoadJSON_EmptyFieldsPreserveDefaults(t *testing.T) {
	tmpDir := t.TempDir()
	jsonPath := filepath.Join(tmpDir, "config.json")

	content := `{
		"server_addr": "5.6.7.8:4700",
		"player_name": "",
		"room_id": ""
	}`
	if err := os.WriteFile(jsonPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write JSON file: %v", err)
	}

	cfg := &Config{PlayerName: "defaultName", RoomID: "defaultRoom"}
	loadJSON(jsonPath, cfg)

	// Empty fields should not overwrite defaults (except password which can be empty)
	if cfg.PlayerName != "defaultName" {
		t.Errorf("expected PlayerName 'defaultName', got '%s'", cfg.PlayerName)
	}
	if cfg.RoomID != "defaultRoom" {
		t.Errorf("expected RoomID 'defaultRoom', got '%s'", cfg.RoomID)
	}
}

func TestLoadConfig_INITakesPrecedence(t *testing.T) {
	// Simulate the LoadConfig priority logic: config.ini > config.json
	tmpDir := t.TempDir()

	iniPath := filepath.Join(tmpDir, "config.ini")
	jsonPath := filepath.Join(tmpDir, "config.json")

	iniContent := "server=ini-server:4700\nname=INIPlayer\nroom=iniroom\n"
	jsonContent := `{"server_addr":"json-server:4700","player_name":"JSONPlayer","room_id":"jsonroom"}`

	if err := os.WriteFile(iniPath, []byte(iniContent), 0644); err != nil {
		t.Fatalf("failed to write INI file: %v", err)
	}
	if err := os.WriteFile(jsonPath, []byte(jsonContent), 0644); err != nil {
		t.Fatalf("failed to write JSON file: %v", err)
	}

	cfg := &Config{PlayerName: "default", RoomID: "default"}
	if !loadINI(iniPath, cfg) {
		loadJSON(jsonPath, cfg)
	}

	// INI values should win
	if cfg.ServerAddr != "ini-server:4700" {
		t.Errorf("expected ServerAddr from INI 'ini-server:4700', got '%s'", cfg.ServerAddr)
	}
	if cfg.PlayerName != "INIPlayer" {
		t.Errorf("expected PlayerName from INI 'INIPlayer', got '%s'", cfg.PlayerName)
	}
	if cfg.RoomID != "iniroom" {
		t.Errorf("expected RoomID from INI 'iniroom', got '%s'", cfg.RoomID)
	}
}

func TestLoadConfig_FallbackToJSON(t *testing.T) {
	// When INI doesn't exist, JSON should be used
	tmpDir := t.TempDir()

	iniPath := filepath.Join(tmpDir, "nonexistent.ini")
	jsonPath := filepath.Join(tmpDir, "config.json")

	jsonContent := `{"server_addr":"json-server:4700","player_name":"JSONPlayer","room_id":"jsonroom"}`
	if err := os.WriteFile(jsonPath, []byte(jsonContent), 0644); err != nil {
		t.Fatalf("failed to write JSON file: %v", err)
	}

	cfg := &Config{PlayerName: "default", RoomID: "default"}
	if !loadINI(iniPath, cfg) {
		loadJSON(jsonPath, cfg)
	}

	if cfg.ServerAddr != "json-server:4700" {
		t.Errorf("expected ServerAddr from JSON 'json-server:4700', got '%s'", cfg.ServerAddr)
	}
	if cfg.PlayerName != "JSONPlayer" {
		t.Errorf("expected PlayerName from JSON 'JSONPlayer', got '%s'", cfg.PlayerName)
	}
	if cfg.RoomID != "jsonroom" {
		t.Errorf("expected RoomID from JSON 'jsonroom', got '%s'", cfg.RoomID)
	}
}

// ── Bonus: handleDataFromServer with mock TunDevice ────────────

func TestHandleDataFromServer(t *testing.T) {
	tunnel, _ := newTestTunnel(t)

	serverIP := net.IPv4(10, 0, 0, 1).To4()
	tunnel.session.serverIP = serverIP
	tunnel.session.serverIPKey.Store(ipKeyPtr(serverIP))

	mock := &mockTunDevice{}
	tunnel.tunDev.Store(mock)

	dp := &protocol.DataPayload{
		SrcIP: net.IPv4(10, 0, 0, 1).To4(),
		DstIP: net.IPv4(10, 0, 0, 2).To4(),
		Data:  []byte{0x45, 0, 0, 20, 0x01, 0x02, 0x03},
	}

	tunnel.handleDataFromServer(dp.Marshal())

	if len(mock.writeBuf) != len(dp.Data) {
		t.Fatalf("expected %d bytes written to TUN, got %d", len(dp.Data), len(mock.writeBuf))
	}
	if !bytes.Equal(mock.writeBuf, dp.Data) {
		t.Errorf("expected data %v, got %v", dp.Data, mock.writeBuf)
	}
}

func TestHandleDataFromServer_EmptyData(t *testing.T) {
	tunnel, _ := newTestTunnel(t)

	mock := &mockTunDevice{}
	tunnel.tunDev.Store(mock)

	dp := &protocol.DataPayload{
		SrcIP: net.IPv4(10, 0, 0, 1).To4(),
		DstIP: net.IPv4(10, 0, 0, 2).To4(),
		Data:  []byte{},
	}

	tunnel.handleDataFromServer(dp.Marshal())

	// Empty data should not be written to TUN
	if len(mock.writeBuf) != 0 {
		t.Errorf("expected 0 bytes written to TUN for empty data, got %d", len(mock.writeBuf))
	}
}

func TestHandleDataFromServer_NilTunDev(t *testing.T) {
	tunnel, _ := newTestTunnel(t)
	// no TUN device — tunDev starts empty (Load() returns nil)

	dp := &protocol.DataPayload{
		SrcIP: net.IPv4(10, 0, 0, 1).To4(),
		DstIP: net.IPv4(10, 0, 0, 2).To4(),
		Data:  []byte{0x01, 0x02},
	}

	// Should not panic
	tunnel.handleDataFromServer(dp.Marshal())
}

// ── 6. IPv6 Tests ─────────────────────────────────────────────

func TestIpKey_NativeIPv6(t *testing.T) {
	ip := net.ParseIP("2408:abcd::1")
	key := netkey.IPKey(ip)
	expected := [16]byte{0x24, 0x08, 0xab, 0xcd, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
	if key != expected {
		t.Errorf("expected %v, got %v", expected, key)
	}
}

func TestIpKey_IPv6Loopback(t *testing.T) {
	ip := net.IPv6loopback
	key := netkey.IPKey(ip)
	expected := [16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
	if key != expected {
		t.Errorf("expected %v, got %v", expected, key)
	}
}

func TestIsLoopback(t *testing.T) {
	tests := []struct {
		ip   net.IP
		want bool
	}{
		{net.IPv4(127, 0, 0, 1), true},
		{net.IPv4(127, 255, 255, 255), true},
		{net.IPv4(10, 0, 0, 1), false},
		{net.IPv6loopback, true},
		{net.ParseIP("2408:abcd::1"), false},
	}
	for _, tt := range tests {
		if got := tt.ip.IsLoopback(); got != tt.want {
			t.Errorf("IsLoopback(%s) = %v, want %v", tt.ip, got, tt.want)
		}
	}
}

func TestHandlePeerInfo_IPv6PublicAddr(t *testing.T) {
	tunnel, _ := newTestTunnel(t)

	// Peer with IPv4 virtual IP but IPv6 public address (typical IPv6 transport scenario)
	peerIP := net.IPv4(10, 0, 0, 5).To4()
	ipv6Addr := &net.UDPAddr{IP: net.ParseIP("2408:abcd::1"), Port: 4700}

	payload := &protocol.PeerInfoPayload{
		Peers: []protocol.PeerInfoEntry{
			{
				VirtualIP:  peerIP,
				PublicAddr: ipv6Addr,
				Username:   "ipv6peer",
			},
		},
	}

	tunnel.handlePeerInfo(context.Background(), payload.Marshal())

	tunnel.mu.RLock()
	peer, ok := tunnel.peers[netkey.IPKey(peerIP)]
	tunnel.mu.RUnlock()

	if !ok {
		t.Fatal("expected peer to be added")
	}
	if peer.PublicAddr.Load().String() != ipv6Addr.String() {
		t.Errorf("expected PublicAddr %s, got %s", ipv6Addr, peer.PublicAddr.Load())
	}
	if peer.Username != "ipv6peer" {
		t.Errorf("expected username 'ipv6peer', got '%s'", peer.Username)
	}
}

// ── ValidateServerAddr Tests ───────────────────────────────────

func TestValidateServerAddr(t *testing.T) {
	tests := []struct {
		addr    string
		wantErr bool
	}{
		{"1.2.3.4:4700", false},
		{"example.com:4700", false},
		{"[2408::1]:4700", false},
		{"", true},
		{"noport", true},
		{"1.2.3.4", true},
		{"1.2.3.4:abc", true},
		{"1.2.3.4:0", true},
		{"1.2.3.4:65535", false},
		{"1.2.3.4:65536", true},
		{"1.2.3.4:99999", true},
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			err := ValidateServerAddr(tt.addr)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateServerAddr(%q) error = %v, wantErr %v", tt.addr, err, tt.wantErr)
			}
		})
	}
}

// ── handleHolePunchReceived Tests ──────────────────────────────

func TestHandleHolePunchReceived(t *testing.T) {
	tunnel, _ := newTestTunnel(t)

	peerIP := net.IPv4(10, 0, 0, 5).To4()
	peerAddr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345}

	tunnel.mu.Lock()
	hpPeer := &Peer{VirtualIP: peerIP, Username: "peer1"}
	hpPeer.PublicAddr.Store(peerAddr)
	tunnel.peers[netkey.IPKey(peerIP)] = hpPeer
	tunnel.mu.Unlock()

	// Build hole punch payload: 4 bytes virtual IP
	payload := make([]byte, 4)
	copy(payload, peerIP.To4())

	// Should not panic and should trigger sendCtrl
	tunnel.handleHolePunchReceived(context.Background(), payload)

	// Verify peer still exists
	tunnel.mu.RLock()
	_, ok := tunnel.peers[netkey.IPKey(peerIP)]
	tunnel.mu.RUnlock()
	if !ok {
		t.Error("peer should still exist after hole punch")
	}
}

func TestHandleHolePunchReceived_UnknownPeer(t *testing.T) {
	tunnel, _ := newTestTunnel(t)

	// Unknown peer IP
	peerIP := net.IPv4(10, 0, 0, 99).To4()
	payload := make([]byte, 4)
	copy(payload, peerIP.To4())

	// Should not panic
	tunnel.handleHolePunchReceived(context.Background(), payload)
}

// ── cleanStalePeers Tests ──────────────────────────────────────

func TestCleanStalePeers(t *testing.T) {
	tunnel, _ := newTestTunnel(t)

	// Add a peer with old lastSeen
	peerIP := net.IPv4(10, 0, 0, 5).To4()
	oldTime := time.Now().Add(-2 * time.Minute) // 2 minutes ago (> 90s grace period)
	peer := &Peer{
		VirtualIP:  peerIP,
		Username:   "stale",
	}
	peer.PublicAddr.Store(&net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345})
	peer.lastSeen.Store(oldTime.UnixNano())

	tunnel.mu.Lock()
	tunnel.peers[netkey.IPKey(peerIP)] = peer
	tunnel.mu.Unlock()

	tunnel.cleanStalePeers()

	tunnel.mu.RLock()
	_, ok := tunnel.peers[netkey.IPKey(peerIP)]
	tunnel.mu.RUnlock()

	if ok {
		t.Error("stale peer should have been removed")
	}
}

func TestCleanStalePeers_KeepsRecent(t *testing.T) {
	tunnel, _ := newTestTunnel(t)

	peerIP := net.IPv4(10, 0, 0, 5).To4()
	recentTime := time.Now().Add(-10 * time.Second) // 10 seconds ago (< 90s grace period)
	peer := &Peer{
		VirtualIP:  peerIP,
		Username:   "recent",
	}
	peer.PublicAddr.Store(&net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345})
	peer.lastSeen.Store(recentTime.UnixNano())

	tunnel.mu.Lock()
	tunnel.peers[netkey.IPKey(peerIP)] = peer
	tunnel.mu.Unlock()

	tunnel.cleanStalePeers()

	tunnel.mu.RLock()
	_, ok := tunnel.peers[netkey.IPKey(peerIP)]
	tunnel.mu.RUnlock()

	if !ok {
		t.Error("recent peer should NOT be removed")
	}
}

// ── decryptWriteAndRelease Tests ───────────────────────────────

func TestDecryptWriteAndRelease_NilTunDev(t *testing.T) {
	tunnel, _ := newTestTunnel(t)
	// no TUN device — tunDev starts empty (Load() returns nil)

	dp := &protocol.DataPayload{
		SrcIP: net.IPv4(10, 0, 0, 1).To4(),
		DstIP: net.IPv4(10, 0, 0, 2).To4(),
		Data:  []byte{0x01, 0x02},
	}

	// Should not panic, should release payload
	tunnel.decryptWriteAndRelease(dp, nil)
}

func TestDecryptWriteAndRelease_NoCipher(t *testing.T) {
	tunnel, _ := newTestTunnel(t)
	mock := &mockTunDevice{}
	tunnel.tunDev.Store(mock)

	dp := &protocol.DataPayload{
		SrcIP: net.IPv4(10, 0, 0, 1).To4(),
		DstIP: net.IPv4(10, 0, 0, 2).To4(),
		Data:  []byte{0x45, 0x00, 0x00, 0x1c},
	}
	dataLen := len(dp.Data) // save before PutDataPayload clears it

	tunnel.decryptWriteAndRelease(dp, nil)

	if len(mock.writeBuf) != dataLen {
		t.Errorf("expected %d bytes written, got %d", dataLen, len(mock.writeBuf))
	}
}

// ── handleDirectData Tests ─────────────────────────────────────

func TestHandleDirectData_UnknownPeer(t *testing.T) {
	tunnel, _ := newTestTunnel(t)
	mock := &mockTunDevice{}
	tunnel.tunDev.Store(mock)

	// Unknown peer
	peerIP := net.IPv4(10, 0, 0, 99).To4()
	fromAddr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345}

	dp := &protocol.DataPayload{
		SrcIP: peerIP,
		DstIP: tunnel.session.virtualIP,
		Data:  []byte{0x01, 0x02},
	}

	msg := &protocol.Message{
		Type:    protocol.TypeData,
		Payload: dp.Marshal(),
	}

	tunnel.handleDirectData(context.Background(), fromAddr, msg)

	// Should not write to TUN
	if len(mock.writeBuf) != 0 {
		t.Error("unknown peer data should not be written to TUN")
	}
}

func TestHandleDirectData_WrongAddress(t *testing.T) {
	tunnel, _ := newTestTunnel(t)
	mock := &mockTunDevice{}
	tunnel.tunDev.Store(mock)

	peerIP := net.IPv4(10, 0, 0, 5).To4()
	correctAddr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345}
	wrongAddr := &net.UDPAddr{IP: net.IPv4(99, 99, 99, 99), Port: 12345}
	wrongPort := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 9999}

	tunnel.mu.Lock()
	wdPeer := &Peer{VirtualIP: peerIP, Username: "peer1"}
	wdPeer.PublicAddr.Store(correctAddr)
	tunnel.peers[netkey.IPKey(peerIP)] = wdPeer
	tunnel.mu.Unlock()

	dp := &protocol.DataPayload{
		SrcIP: peerIP,
		DstIP: tunnel.session.virtualIP,
		Data:  []byte{0x01, 0x02},
	}
	msg := &protocol.Message{Type: protocol.TypeData, Payload: dp.Marshal()}

	// Wrong IP
	tunnel.handleDirectData(context.Background(), wrongAddr, msg)
	if len(mock.writeBuf) != 0 {
		t.Error("wrong IP should not write to TUN")
	}

	// Wrong port
	tunnel.handleDirectData(context.Background(), wrongPort, msg)
	if len(mock.writeBuf) != 0 {
		t.Error("wrong port should not write to TUN")
	}
}

// ── handleDataFromServer Tests ─────────────────────────────────

func TestHandleDataFromServer_AnySrcIP(t *testing.T) {
	tunnel, _ := newTestTunnel(t)
	mock := &mockTunDevice{}
	tunnel.tunDev.Store(mock)

	// Unknown srcIP (not server, not peer) — should be accepted because
	// the server already validates anti-spoofing for relayed packets.
	unknownIP := net.IPv4(99, 99, 99, 99).To4()
	dp := &protocol.DataPayload{
		SrcIP: unknownIP,
		DstIP: net.IPv4(10, 0, 0, 2).To4(),
		Data:  []byte{0x01, 0x02},
	}

	tunnel.handleDataFromServer(dp.Marshal())

	// Should write to TUN (server-relayed packets are trusted unconditionally)
	if len(mock.writeBuf) == 0 {
		t.Error("relayed packet with unknown srcIP should be written to TUN")
	}
}

func TestHandleDataFromServer_EncryptedRelay(t *testing.T) {
	tunnel, _ := newTestTunnel(t)
	mock := &mockTunDevice{}
	tunnel.tunDev.Store(mock)

	key := make([]byte, crypto.KeySize)
	for i := range key {
		key[i] = byte(i)
	}

	c, err := crypto.NewCipher(key, crypto.DirClientToClient)
	if err != nil {
		t.Fatalf("failed to create cipher: %v", err)
	}

	plaintext := []byte{0x45, 0x00, 0x00, 0x1c, 0xab, 0xcd}
	encrypted := c.Encrypt(plaintext)

	dp := &protocol.DataPayload{
		SrcIP: net.IPv4(10, 0, 0, 1).To4(),
		DstIP: net.IPv4(10, 0, 0, 2).To4(),
		Data:  encrypted,
	}

	tunnel.crypto.p2pCipher = c
	tunnel.handleDataFromServer(dp.Marshal())

	if !bytes.Equal(mock.writeBuf, plaintext) {
		t.Errorf("expected decrypted data %v, got %v", plaintext, mock.writeBuf)
	}
}

// ── markServerResponse Tests ───────────────────────────────────

func TestMarkServerResponse(t *testing.T) {
	tunnel, _ := newTestTunnel(t)

	// Initially zero (no response yet)
	if tunnel.lastServerResponse.Load() != 0 {
		t.Error("expected zero initially")
	}

	tunnel.markServerResponse()

	now := time.Now()
	lastSeen := tunnel.lastServerResponse.Load()
	if lastSeen == 0 {
		t.Fatal("expected non-zero after markServerResponse")
	}
	if now.Sub(time.Unix(0, lastSeen)) > time.Second {
		t.Error("timestamp should be recent")
	}
}

// ── hasDirectPeerTraffic Tests ─────────────────────────────────

func TestHasDirectPeerTraffic_DirectReach(t *testing.T) {
	tunnel, _ := newTestTunnel(t)

	peerIP := net.IPv4(10, 0, 0, 5).To4()
	peer := &Peer{
		VirtualIP:  peerIP,
		Username:   "peer1",
	}
	peer.PublicAddr.Store(&net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345})
	peer.DirectReach.Store(true)

	tunnel.mu.Lock()
	tunnel.peers[netkey.IPKey(peerIP)] = peer
	tunnel.mu.Unlock()

	if !tunnel.hasDirectPeerTraffic(peerIP) {
		t.Error("expected true for peer with DirectReach")
	}
}

func TestHasDirectPeerTraffic_NoDirectReach(t *testing.T) {
	tunnel, _ := newTestTunnel(t)

	peerIP := net.IPv4(10, 0, 0, 5).To4()
	peer := &Peer{
		VirtualIP:  peerIP,
		Username:   "peer1",
	}
	peer.PublicAddr.Store(&net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345})

	tunnel.mu.Lock()
	tunnel.peers[netkey.IPKey(peerIP)] = peer
	tunnel.mu.Unlock()

	if tunnel.hasDirectPeerTraffic(peerIP) {
		t.Error("expected false for peer without DirectReach")
	}
}

func TestHasDirectPeerTraffic_UnknownPeer(t *testing.T) {
	tunnel, _ := newTestTunnel(t)

	peerIP := net.IPv4(10, 0, 0, 99).To4()

	if tunnel.hasDirectPeerTraffic(peerIP) {
		t.Error("expected false for unknown peer")
	}
}

// ── sendP2PKeepalives Tests ────────────────────────────────────

func TestSendP2PKeepalives_NoPeers(t *testing.T) {
	tunnel, _ := newTestTunnel(t)

	// Should not panic
	tunnel.sendP2PKeepalives()
}

func TestSendP2PKeepalives_WithDirectPeers(t *testing.T) {
	tunnel, _ := newTestTunnel(t)

	peerIP := net.IPv4(10, 0, 0, 5).To4()
	peerAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}

	peer := &Peer{
		VirtualIP:  peerIP,
		Username:   "peer1",
	}
	peer.PublicAddr.Store(peerAddr)
	peer.DirectReach.Store(true)

	tunnel.mu.Lock()
	tunnel.peers[netkey.IPKey(peerIP)] = peer
	tunnel.nat.cachedPunchPacket.Store(protocol.EncodeChecked(protocol.TypeHolePunch, tunnel.session.virtualIP.To4()))
	tunnel.mu.Unlock()

	// Should not panic
	tunnel.sendP2PKeepalives()
}

// ── Tunnel Status Tests ────────────────────────────────────────

func TestTunnelStatus_Connected(t *testing.T) {
	tunnel, _ := newTestTunnel(t)
	mock := &mockTunDevice{}
	tunnel.tunDev.Store(mock)
	tunnel.session.virtualIP = net.IPv4(10, 10, 0, 2).To4()
	tunnel.session.subnetMask = net.CIDRMask(24, 32)
	tunnel.session.serverIP = net.IPv4(10, 10, 0, 1).To4()

	status := tunnel.Status()

	if !status.Connected {
		t.Error("expected Connected to be true")
	}
	if !status.VirtualIP.Equal(tunnel.session.virtualIP) {
		t.Errorf("VirtualIP: got %v, want %v", status.VirtualIP, tunnel.session.virtualIP)
	}
	if !status.ServerIP.Equal(tunnel.session.serverIP) {
		t.Errorf("ServerIP: got %v, want %v", status.ServerIP, tunnel.session.serverIP)
	}
}

func TestTunnelStatus_Disconnected(t *testing.T) {
	tunnel, _ := newTestTunnel(t)

	status := tunnel.Status()

	if status.Connected {
		t.Error("expected Connected to be false")
	}
}

func TestTunnelStatus_WithPeers(t *testing.T) {
	tunnel, _ := newTestTunnel(t)
	mock := &mockTunDevice{}
	tunnel.tunDev.Store(mock)
	tunnel.session.virtualIP = net.IPv4(10, 10, 0, 2).To4()

	// Add P2P peer
	peer1IP := net.IPv4(10, 0, 0, 5).To4()
	peer1 := &Peer{VirtualIP: peer1IP, Username: "p2p"}
	peer1.DirectReach.Store(true)

	// Add relay peer
	peer2IP := net.IPv4(10, 0, 0, 6).To4()
	peer2 := &Peer{VirtualIP: peer2IP, Username: "relay"}

	tunnel.mu.Lock()
	tunnel.peers[netkey.IPKey(peer1IP)] = peer1
	tunnel.peers[netkey.IPKey(peer2IP)] = peer2
	tunnel.mu.Unlock()

	status := tunnel.Status()

	if status.PeerCount != 2 {
		t.Errorf("PeerCount: got %d, want 2", status.PeerCount)
	}
	if status.P2PPeers != 1 {
		t.Errorf("P2PPeers: got %d, want 1", status.P2PPeers)
	}
	if status.RelayPeers != 1 {
		t.Errorf("RelayPeers: got %d, want 1", status.RelayPeers)
	}
}

// ── Server Version Tests ───────────────────────────────────────

func TestTunnelStatus_ServerVersion(t *testing.T) {
	tunnel, _ := newTestTunnel(t)
	mock := &mockTunDevice{}
	tunnel.tunDev.Store(mock)
	tunnel.session.virtualIP = net.IPv4(10, 10, 0, 2).To4()
	tunnel.session.serverVersion.Store(0x0102) // v1.2

	status := tunnel.Status()

	if status.ServerVersion != 0x0102 {
		t.Errorf("ServerVersion: got 0x%04x, want 0x0102", status.ServerVersion)
	}
}

func TestTunnelStatus_ServerVersionZero(t *testing.T) {
	tunnel, _ := newTestTunnel(t)
	mock := &mockTunDevice{}
	tunnel.tunDev.Store(mock)
	tunnel.session.virtualIP = net.IPv4(10, 10, 0, 2).To4()
	tunnel.session.serverVersion.Store(0) // old server

	status := tunnel.Status()

	if status.ServerVersion != 0 {
		t.Errorf("ServerVersion: got 0x%04x, want 0", status.ServerVersion)
	}
}

func TestHandleAssignIP_StoresVersion(t *testing.T) {
	tunnel, _ := newTestTunnel(t)

	assign := &protocol.AssignIPPayload{
		VirtualIP:  net.IPv4(10, 10, 0, 5).To4(),
		SubnetMask: net.CIDRMask(24, 32),
		ServerIP:   net.IPv4(10, 10, 0, 1).To4(),
		Version:    protocol.AppVersion, // same as client — compatible
	}

	err := tunnel.handleAssignIP(assign.Marshal())
	if err != nil {
		t.Fatalf("handleAssignIP failed: %v", err)
	}

	if 	tunnel.session.serverVersion.Load() != uint32(protocol.AppVersion) {
		t.Errorf("serverVersion: got 0x%04x, want 0x%04x", tunnel.session.serverVersion.Load(), protocol.AppVersion)
	}
}

func TestVirtualIP(t *testing.T) {
	tunnel, _ := newTestTunnel(t)

	// Initially nil
	if tunnel.VirtualIP() != nil {
		t.Error("expected nil initially")
	}

	ip := net.IPv4(10, 10, 0, 2).To4()
	tunnel.session.virtualIP = ip

	if !tunnel.VirtualIP().Equal(ip) {
		t.Errorf("got %v, want %v", tunnel.VirtualIP(), ip)
	}
}

// ── CloseTUN Tests ─────────────────────────────────────────────

func TestCloseTUN(t *testing.T) {
	tunnel, _ := newTestTunnel(t)
	mock := &mockTunDevice{}
	tunnel.tunDev.Store(mock)
	tunnel.lastAssignedIP = net.IPv4(10, 10, 0, 2).To4()

	tunnel.CloseTUN()

	if !mock.closed {
		t.Error("expected TUN device to be closed")
	}
	if tunnel.lastAssignedIP != nil {
		t.Error("expected lastAssignedIP to be nil")
	}
}

func TestCloseTUN_NilDevice(t *testing.T) {
	tunnel, _ := newTestTunnel(t)
	// tunDev starts empty (Load() returns nil)

	// Should not panic
	tunnel.CloseTUN()
}

// ── Disconnect Tests ───────────────────────────────────────────

func TestDisconnect(t *testing.T) {
	tunnel, _ := newTestTunnel(t)
	mock := &mockTunDevice{}
	tunnel.tunDev.Store(mock)
	tunnel.serverAddr.Store(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4700})

	tunnel.Disconnect()

	// Disconnect should be idempotent
	tunnel.Disconnect()
}

// ── loadINI / loadJSON Tests ───────────────────────────────────

func TestLoadINI_AllFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.ini")

	content := `# Comment
server=1.2.3.4:4700
name=TestPlayer
room=myroom
password=secret123
lang=en
mtu=1200
`
	os.WriteFile(path, []byte(content), 0644)

	cfg := &Config{}
	ok := loadINI(path, cfg)

	if !ok {
		t.Fatal("loadINI should return true")
	}
	if cfg.ServerAddr != "1.2.3.4:4700" {
		t.Errorf("ServerAddr: got %q", cfg.ServerAddr)
	}
	if cfg.PlayerName != "TestPlayer" {
		t.Errorf("PlayerName: got %q", cfg.PlayerName)
	}
	if cfg.RoomID != "myroom" {
		t.Errorf("RoomID: got %q", cfg.RoomID)
	}
	if cfg.RoomPassword != "secret123" {
		t.Errorf("RoomPassword: got %q", cfg.RoomPassword)
	}
	if cfg.Lang != "en" {
		t.Errorf("Lang: got %q", cfg.Lang)
	}
	if cfg.MTU != 1200 {
		t.Errorf("MTU: got %d", cfg.MTU)
	}
}

func TestLoadINI_MtuOutOfRange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.ini")

	// MTU too low
	os.WriteFile(path, []byte("mtu=100"), 0644)
	cfg := &Config{MTU: 1400}
	loadINI(path, cfg)
	if cfg.MTU != 1400 {
		t.Errorf("MTU should be unchanged for out-of-range value, got %d", cfg.MTU)
	}

	// MTU too high
	os.WriteFile(path, []byte("mtu=99999"), 0644)
	cfg.MTU = 1400
	loadINI(path, cfg)
	if cfg.MTU != 1400 {
		t.Errorf("MTU should be unchanged for out-of-range value, got %d", cfg.MTU)
	}
}

func TestLoadJSON_AllFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	content := `{
		"server_addr": "5.6.7.8:4700",
		"player_name": "JSONPlayer",
		"room_id": "jsonroom",
		"room_password": "jsonpass",
		"lang": "en",
		"mtu": 1300
	}`
	os.WriteFile(path, []byte(content), 0644)

	cfg := &Config{MTU: 1400}
	loadJSON(path, cfg)

	if cfg.ServerAddr != "5.6.7.8:4700" {
		t.Errorf("ServerAddr: got %q", cfg.ServerAddr)
	}
	if cfg.PlayerName != "JSONPlayer" {
		t.Errorf("PlayerName: got %q", cfg.PlayerName)
	}
	if cfg.RoomID != "jsonroom" {
		t.Errorf("RoomID: got %q", cfg.RoomID)
	}
	if cfg.RoomPassword != "jsonpass" {
		t.Errorf("RoomPassword: got %q", cfg.RoomPassword)
	}
	if cfg.Lang != "en" {
		t.Errorf("Lang: got %q", cfg.Lang)
	}
	if cfg.MTU != 1300 {
		t.Errorf("MTU: got %d", cfg.MTU)
	}
}

func TestSaveConfig(t *testing.T) {
	cfg := &Config{
		ServerAddr:   "1.2.3.4:4700",
		PlayerName:   "TestPlayer",
		RoomID:       "testroom",
		RoomPassword: "pass",
		Lang:         "en",
		MTU:          1200,
	}

	err := SaveConfig(cfg)
	if err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}

	// Clean up
	defer os.Remove(PortableConfigPath())
}

// ── ValidateServerAddr Edge Cases ──────────────────────────────

func TestValidateServerAddr_IPv6(t *testing.T) {
	tests := []struct {
		addr    string
		wantErr bool
	}{
		{"[2408::1]:4700", false},
		{"[::1]:4700", false},
		{"2408::1:4700", true}, // missing brackets
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			err := ValidateServerAddr(tt.addr)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateServerAddr(%q) error = %v, wantErr %v", tt.addr, err, tt.wantErr)
			}
		})
	}
}

func TestValidateServerAddr_NonNumericPort(t *testing.T) {
	err := ValidateServerAddr("1.2.3.4:abc")
	if err == nil {
		t.Error("expected error for non-numeric port")
	}
}

// ── sendUDP Tests ──────────────────────────────────────────────

func TestSendUDP_Normal(t *testing.T) {
	tunnel, serverConn := newTestTunnel(t)

	data := []byte{0x01, 0x02, 0x03}
	tunnel.sendUDP(data, tunnel.serverAddr.Load())

	pkt := readUDPWithTimeout(serverConn, 100*time.Millisecond)
	if pkt == nil {
		t.Fatal("expected packet")
	}
	if !bytes.Equal(pkt, data) {
		t.Errorf("data mismatch: got %v, want %v", pkt, data)
	}
}

// ── sendCtrl Tests ─────────────────────────────────────────────

func TestSendCtrl_Normal(t *testing.T) {
	tunnel, serverConn := newTestTunnel(t)

	data := []byte{0x01, 0x02}
	tunnel.sendCtrl(data, tunnel.serverAddr.Load())

	pkt := readUDPWithTimeout(serverConn, 100*time.Millisecond)
	if pkt == nil {
		t.Fatal("expected packet")
	}
}

// ── sendLoop Tests ─────────────────────────────────────────────

func TestSendLoop_CtrlPriority(t *testing.T) {
	tunnel, serverConn := newTestTunnel(t)

	// Send data first, then ctrl
	tunnel.sendCh <- sendJob{data: []byte{0x01}, addr: tunnel.serverAddr.Load()}
	tunnel.ctrlCh <- sendJob{data: []byte{0x02}, addr: tunnel.serverAddr.Load()}

	// Both should be received
	pkt1 := readUDPWithTimeout(serverConn, 100*time.Millisecond)
	if pkt1 == nil {
		t.Fatal("expected first packet")
	}

	pkt2 := readUDPWithTimeout(serverConn, 100*time.Millisecond)
	if pkt2 == nil {
		t.Fatal("expected second packet")
	}
}

// ── handleDirectData More Tests ────────────────────────────────

func TestHandleDirectData_NonTypeData(t *testing.T) {
	tunnel, _ := newTestTunnel(t)
	mock := &mockTunDevice{}
	tunnel.tunDev.Store(mock)

	fromAddr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345}

	msg := &protocol.Message{
		Type:    protocol.TypeKeepAlive, // not TypeData
		Payload: []byte{},
	}

	tunnel.handleDirectData(context.Background(), fromAddr, msg)

	if len(mock.writeBuf) != 0 {
		t.Error("non-TypeData should not write to TUN")
	}
}

func TestHandleDirectData_EmptyPayload(t *testing.T) {
	tunnel, _ := newTestTunnel(t)
	mock := &mockTunDevice{}
	tunnel.tunDev.Store(mock)

	fromAddr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345}

	dp := &protocol.DataPayload{
		SrcIP: net.IPv4(10, 0, 0, 5).To4(),
		DstIP: tunnel.session.virtualIP,
		Data:  []byte{}, // empty
	}

	msg := &protocol.Message{
		Type:    protocol.TypeData,
		Payload: dp.Marshal(),
	}

	tunnel.handleDirectData(context.Background(), fromAddr, msg)

	if len(mock.writeBuf) != 0 {
		t.Error("empty data should not write to TUN")
	}
}

func TestHandleDirectData_TokenValidation_OldFormat(t *testing.T) {
	tunnel, _ := newTestTunnel(t)
	mock := &mockTunDevice{}
	tunnel.tunDev.Store(mock)

	peerIP := net.IPv4(10, 0, 0, 5).To4()
	fromAddr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345}

	tunnel.mu.Lock()
	tunnel.session.sessionToken = [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	wdPeer := &Peer{VirtualIP: peerIP, Username: "peer1"}
	wdPeer.PublicAddr.Store(fromAddr)
	tunnel.peers[netkey.IPKey(peerIP)] = wdPeer
	tunnel.mu.Unlock()

	// Old format payload with valid token: srcIP(4) + dstIP(4) + flags(1) + token(16) + data
	payload := make([]byte, 4+4+1+16+5)
	copy(payload[0:4], peerIP)
	copy(payload[4:8], tunnel.session.virtualIP)
	payload[8] = protocol.DataFlagHasToken
	copy(payload[9:25], tunnel.session.sessionToken[:])
	copy(payload[25:], []byte("hello"))

	msg := &protocol.Message{Type: protocol.TypeData, Payload: payload}

	tunnel.handleDirectData(context.Background(), fromAddr, msg)

	if len(mock.writeBuf) == 0 {
		t.Error("valid old-format token data should write to TUN")
	}
}

func TestHandleDirectData_TokenValidation_NewFormat(t *testing.T) {
	tunnel, _ := newTestTunnel(t)
	mock := &mockTunDevice{}
	tunnel.tunDev.Store(mock)

	peerIP := net.IPv4(10, 0, 0, 5).To4()
	fromAddr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345}

	tunnel.mu.Lock()
	tunnel.session.sessionToken = [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	wdPeer := &Peer{VirtualIP: peerIP, Username: "peer1"}
	wdPeer.PublicAddr.Store(fromAddr)
	tunnel.peers[netkey.IPKey(peerIP)] = wdPeer
	tunnel.mu.Unlock()

	// New format payload with valid token: srcIP(4) + dstIP(4) + formatVer(1) + flags(1) + token(16) + data
	payload := make([]byte, 4+4+1+1+16+5)
	copy(payload[0:4], peerIP)
	copy(payload[4:8], tunnel.session.virtualIP)
	payload[8] = protocol.DataFormatVersion
	payload[9] = protocol.DataFlagHasToken
	copy(payload[10:26], tunnel.session.sessionToken[:])
	copy(payload[26:], []byte("hello"))

	msg := &protocol.Message{Type: protocol.TypeData, Payload: payload}

	tunnel.handleDirectData(context.Background(), fromAddr, msg)

	if len(mock.writeBuf) == 0 {
		t.Error("valid new-format token data should write to TUN")
	}
}

func TestHandleDirectData_TokenValidation_WrongToken(t *testing.T) {
	tunnel, _ := newTestTunnel(t)
	mock := &mockTunDevice{}
	tunnel.tunDev.Store(mock)

	peerIP := net.IPv4(10, 0, 0, 5).To4()
	fromAddr := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 12345}

	tunnel.mu.Lock()
	tunnel.session.sessionToken = [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	wdPeer := &Peer{VirtualIP: peerIP, Username: "peer1"}
	wdPeer.PublicAddr.Store(fromAddr)
	tunnel.peers[netkey.IPKey(peerIP)] = wdPeer
	tunnel.mu.Unlock()

	// New format with WRONG token
	payload := make([]byte, 4+4+1+1+16+5)
	copy(payload[0:4], peerIP)
	copy(payload[4:8], tunnel.session.virtualIP)
	payload[8] = protocol.DataFormatVersion
	payload[9] = protocol.DataFlagHasToken
	// Explicitly zero the token field so it won't match sessionToken
	clear(payload[10:26])
	copy(payload[26:], []byte("hello"))

	msg := &protocol.Message{Type: protocol.TypeData, Payload: payload}

	tunnel.handleDirectData(context.Background(), fromAddr, msg)

	if len(mock.writeBuf) != 0 {
		t.Error("wrong token data should NOT write to TUN")
	}
}

// ── handleServerData Tests ─────────────────────────────────────

func TestHandleServerData_Ping(t *testing.T) {
	tunnel, serverConn := newTestTunnel(t)

	// Build a ping payload
	ping := &protocol.PingPayload{Timestamp: time.Now().UnixNano()}
	msg := &protocol.Message{
		Type:    protocol.TypePing,
		Payload: ping.Marshal(),
	}

	tunnel.handleServerData(context.Background(), tunnel.conn, msg)

	// Should have sent a pong
	pkt := readUDPWithTimeout(serverConn, 100*time.Millisecond)
	if pkt == nil {
		t.Error("expected pong packet")
	}
}

func TestHandleServerData_MarkServerResponse(t *testing.T) {
	tunnel, _ := newTestTunnel(t)

	// Initially zero (no response yet)
	if tunnel.lastServerResponse.Load() != 0 {
		t.Error("expected zero initially")
	}

	msg := &protocol.Message{
		Type:    protocol.TypePing,
		Payload: (&protocol.PingPayload{Timestamp: time.Now().UnixNano()}).Marshal(),
	}

	tunnel.handleServerData(context.Background(), tunnel.conn, msg)

	// Should have marked server response
	if tunnel.lastServerResponse.Load() == 0 {
		t.Error("expected server response to be marked")
	}
}

// ── handleAssignIP Tests ───────────────────────────────────────

func TestHandleAssignIP_Valid(t *testing.T) {
	tunnel, _ := newTestTunnel(t)

	assign := &protocol.AssignIPPayload{
		VirtualIP:  net.IPv4(10, 10, 0, 5).To4(),
		SubnetMask: net.CIDRMask(24, 32),
		ServerIP:   net.IPv4(10, 10, 0, 1).To4(),
		Version:    protocol.AppVersion,
	}

	err := tunnel.handleAssignIP(assign.Marshal())
	if err != nil {
		t.Fatalf("handleAssignIP failed: %v", err)
	}

	if !tunnel.session.virtualIP.Equal(net.IPv4(10, 10, 0, 5)) {
		t.Errorf("VirtualIP: got %v", tunnel.session.virtualIP)
	}
	if !tunnel.session.serverIP.Equal(net.IPv4(10, 10, 0, 1)) {
		t.Errorf("ServerIP: got %v", tunnel.session.serverIP)
	}
}

func TestHandleAssignIP_VersionIncompatible(t *testing.T) {
	tunnel, _ := newTestTunnel(t)

	assign := &protocol.AssignIPPayload{
		VirtualIP:  net.IPv4(10, 10, 0, 5).To4(),
		SubnetMask: net.CIDRMask(24, 32),
		ServerIP:   net.IPv4(10, 10, 0, 1).To4(),
		Version:    0xFF00, // different major version
	}

	err := tunnel.handleAssignIP(assign.Marshal())
	if err == nil {
		t.Error("expected error for incompatible version")
	}
}

func TestHandleAssignIP_InvalidIP(t *testing.T) {
	tunnel, _ := newTestTunnel(t)

	// IP not in subnet
	assign := &protocol.AssignIPPayload{
		VirtualIP:  net.IPv4(192, 168, 1, 1).To4(), // different subnet
		SubnetMask: net.CIDRMask(24, 32),
		ServerIP:   net.IPv4(10, 10, 0, 1).To4(),
	}

	err := tunnel.handleAssignIP(assign.Marshal())
	if err == nil {
		t.Error("expected error for IP not in subnet")
	}
}

// ── startHolePunch Tests ───────────────────────────────────────

func TestStartHolePunch_UnknownPeer(t *testing.T) {
	tunnel, _ := newTestTunnel(t)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Should not panic for unknown peer
	tunnel.startHolePunch(ctx, net.IPv4(10, 0, 0, 99).To4())
}

func TestStartHolePunch_ContextCancel(t *testing.T) {
	tunnel, _ := newTestTunnel(t)

	peerIP := net.IPv4(10, 0, 0, 5).To4()
	peer := &Peer{
		VirtualIP:  peerIP,
		Username:   "peer1",
	}
	peer.PublicAddr.Store(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345})

	tunnel.mu.Lock()
	tunnel.peers[netkey.IPKey(peerIP)] = peer
	tunnel.nat.cachedPunchPacket.Store(protocol.EncodeChecked(protocol.TypeHolePunch, tunnel.session.virtualIP.To4()))
	tunnel.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	// Should return quickly
	done := make(chan struct{})
	go func() {
		tunnel.startHolePunch(ctx, peerIP)
		close(done)
	}()

	select {
	case <-done:
		// expected
	case <-time.After(1 * time.Second):
		t.Error("startHolePunch should return when context is cancelled")
	}
}

// ── retryFailedHolePunches Tests ───────────────────────────────

func TestRetryFailedHolePunches_NoPeers(t *testing.T) {
	tunnel, _ := newTestTunnel(t)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Should not panic
	tunnel.retryFailedHolePunches(ctx)
}

func TestRetryFailedHolePunches_WithPeers(t *testing.T) {
	tunnel, _ := newTestTunnel(t)

	peerIP := net.IPv4(10, 0, 0, 5).To4()
	peer := &Peer{
		VirtualIP:  peerIP,
		Username:   "peer1",
	}
	peer.PublicAddr.Store(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345})
	// DirectReach is false by default

	tunnel.mu.Lock()
	tunnel.peers[netkey.IPKey(peerIP)] = peer
	tunnel.nat.cachedPunchPacket.Store(protocol.EncodeChecked(protocol.TypeHolePunch, tunnel.session.virtualIP.To4()))
	tunnel.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Should not panic
	tunnel.retryFailedHolePunches(ctx)
}

// ── routePacket Tests ──────────────────────────────────────────

func TestRoutePacket_BroadcastToServer(t *testing.T) {
	tunnel, serverConn := newTestTunnel(t)
	mock := &mockTunDevice{}
	tunnel.tunDev.Store(mock)
	tunnel.session.virtualIP = net.IPv4(10, 10, 0, 2).To4()
	tunnel.session.subnetMask = net.CIDRMask(24, 32)
	tunnel.session.serverIP = net.IPv4(10, 10, 0, 1).To4()
	tunnel.session.serverIPKey.Store(ipKeyPtr(tunnel.session.serverIP))
	tunnel.session.cachedSubnet.Store(&net.IPNet{
		IP:   tunnel.session.virtualIP.Mask(tunnel.session.subnetMask),
		Mask: tunnel.session.subnetMask,
	})

	// Broadcast packet (255.255.255.255)
	pkt := make([]byte, 20)
	pkt[0] = 0x45 // IPv4
	srcIP := net.IPv4(10, 10, 0, 2).To4()
	dstIP := net.IPv4(255, 255, 255, 255).To4()
	var srcIP4, dstIP4 [4]byte
	copy(srcIP4[:], srcIP)
	copy(dstIP4[:], dstIP)

	tunnel.routePacket(pkt, srcIP4, dstIP4)

	// Should have sent to server
	pkt2 := readUDPWithTimeout(serverConn, 100*time.Millisecond)
	if pkt2 == nil {
		t.Error("expected broadcast packet to be sent to server")
	}
}

func TestRoutePacket_UnicastToServer(t *testing.T) {
	tunnel, serverConn := newTestTunnel(t)
	mock := &mockTunDevice{}
	tunnel.tunDev.Store(mock)
	tunnel.session.virtualIP = net.IPv4(10, 10, 0, 2).To4()
	tunnel.session.subnetMask = net.CIDRMask(24, 32)
	tunnel.session.serverIP = net.IPv4(10, 10, 0, 1).To4()
	tunnel.session.serverIPKey.Store(ipKeyPtr(tunnel.session.serverIP))
	tunnel.session.cachedSubnet.Store(&net.IPNet{
		IP:   tunnel.session.virtualIP.Mask(tunnel.session.subnetMask),
		Mask: tunnel.session.subnetMask,
	})

	// Unicast to server
	pkt := make([]byte, 20)
	pkt[0] = 0x45
	srcIP := net.IPv4(10, 10, 0, 2).To4()
	dstIP := net.IPv4(10, 10, 0, 1).To4() // server IP
	var srcIP4, dstIP4 [4]byte
	copy(srcIP4[:], srcIP)
	copy(dstIP4[:], dstIP)

	tunnel.routePacket(pkt, srcIP4, dstIP4)

	pkt2 := readUDPWithTimeout(serverConn, 100*time.Millisecond)
	if pkt2 == nil {
		t.Error("expected packet to be sent to server")
	}
}

func TestRoutePacket_PeerFallbackToRelay(t *testing.T) {
	tunnel, serverConn := newTestTunnel(t)
	mock := &mockTunDevice{}
	tunnel.tunDev.Store(mock)
	tunnel.session.virtualIP = net.IPv4(10, 10, 0, 2).To4()
	tunnel.session.subnetMask = net.CIDRMask(24, 32)
	tunnel.session.serverIP = net.IPv4(10, 10, 0, 1).To4()
	tunnel.session.serverIPKey.Store(ipKeyPtr(tunnel.session.serverIP))
	tunnel.session.cachedSubnet.Store(&net.IPNet{
		IP:   tunnel.session.virtualIP.Mask(tunnel.session.subnetMask),
		Mask: tunnel.session.subnetMask,
	})

	// Peer exists but no DirectReach - should fallback to relay
	peerIP := net.IPv4(10, 10, 0, 5).To4()
	peer := &Peer{
		VirtualIP:  peerIP,
		Username:   "peer1",
	}
	peer.PublicAddr.Store(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345})
	// DirectReach is false

	tunnel.mu.Lock()
	tunnel.peers[netkey.IPKey(peerIP)] = peer
	tunnel.mu.Unlock()

	pkt := make([]byte, 20)
	pkt[0] = 0x45
	srcIP := net.IPv4(10, 10, 0, 2).To4()
	dstIP := peerIP
	var srcIP4, dstIP4 [4]byte
	copy(srcIP4[:], srcIP)
	copy(dstIP4[:], dstIP)

	tunnel.routePacket(pkt, srcIP4, dstIP4)

	// Should have sent to server (relay)
	pkt2 := readUDPWithTimeout(serverConn, 100*time.Millisecond)
	if pkt2 == nil {
		t.Error("expected packet to be relayed to server")
	}
}

// ── Disconnect Tests ───────────────────────────────────────────

func TestDisconnect_ClosesConn(t *testing.T) {
	tunnel, _ := newTestTunnel(t)
	mock := &mockTunDevice{}
	tunnel.tunDev.Store(mock)

	tunnel.Disconnect()

	// Disconnect should be idempotent
	tunnel.Disconnect()
}
