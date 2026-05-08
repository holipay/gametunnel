package client

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/holipay/gametunnel-protocol/protocol"
)

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
	tunnel.serverAddr = serverConn.LocalAddr().(*net.UDPAddr)

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

// ── 1. ip4Key Tests ────────────────────────────────────────────

func TestIp4Key_NormalIPv4(t *testing.T) {
	ip := net.IPv4(192, 168, 1, 1).To4()
	key := ip4Key(ip)
	expected := [4]byte{192, 168, 1, 1}
	if key != expected {
		t.Errorf("expected %v, got %v", expected, key)
	}
}

func TestIp4Key_IPv4Mapped(t *testing.T) {
	// 16-byte IPv4-mapped address: ::ffff:192.168.1.1
	ip16 := net.IP{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 192, 168, 1, 1}
	key := ip4Key(ip16)
	expected := [4]byte{192, 168, 1, 1}
	if key != expected {
		t.Errorf("expected %v, got %v", expected, key)
	}
}

func TestIp4Key_Consistency(t *testing.T) {
	// Both 4-byte and 16-byte representations of the same IP must produce the same key
	ip4 := net.IPv4(10, 0, 0, 1).To4()
	ip16 := net.IP{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 10, 0, 0, 1}
	if ip4Key(ip4) != ip4Key(ip16) {
		t.Error("4-byte and 16-byte IPv4-mapped should produce the same key")
	}
}

// ── 2. routePacket Tests ───────────────────────────────────────

func TestRoutePacket_Broadcast(t *testing.T) {
	tunnel, serverConn := newTestTunnel(t)

	tunnel.serverIP = net.IPv4(10, 0, 0, 1).To4()
	tunnel.serverIP4 = ip4Key(tunnel.serverIP)
	tunnel.cachedSubnet = &net.IPNet{
		IP:   net.IPv4(10, 0, 0, 0).To4(),
		Mask: net.CIDRMask(24, 32),
	}

	pkt := []byte{0x45, 0, 0, 20, 0, 0, 0, 0, 64, 17, 0, 0, 10, 0, 0, 2, 255, 255, 255, 255}
	srcIP := net.IPv4(10, 0, 0, 2).To4()
	dstIP := net.IPv4(255, 255, 255, 255).To4()

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
	tunnel.serverIP = serverIP
	tunnel.serverIP4 = ip4Key(serverIP)

	pkt := []byte{0x45, 0, 0, 20}
	srcIP := net.IPv4(10, 0, 0, 2).To4()

	go tunnel.routePacket(pkt, srcIP, serverIP)

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
	tunnel.serverIP = serverIP
	tunnel.serverIP4 = ip4Key(serverIP)

	peerIP := net.IPv4(10, 0, 0, 3).To4()
	peerAddr := peerConn.LocalAddr().(*net.UDPAddr)
	tunnel.peers = map[[4]byte]*Peer{
		ip4Key(peerIP): {
			VirtualIP:  peerIP,
			PublicAddr: peerAddr,
			Username:   "peer1",
		},
	}

	pkt := []byte{0x45, 0, 0, 20}
	srcIP := net.IPv4(10, 0, 0, 2).To4()

	go tunnel.routePacket(pkt, srcIP, peerIP)

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
	tunnel.serverIP = serverIP
	tunnel.serverIP4 = ip4Key(serverIP)

	// Peer exists but has no PublicAddr (hole punch not yet completed)
	peerIP := net.IPv4(10, 0, 0, 3).To4()
	tunnel.peers = map[[4]byte]*Peer{
		ip4Key(peerIP): {
			VirtualIP:  peerIP,
			PublicAddr: nil,
			Username:   "peer1",
		},
	}

	pkt := []byte{0x45, 0, 0, 20}
	srcIP := net.IPv4(10, 0, 0, 2).To4()

	go tunnel.routePacket(pkt, srcIP, peerIP)

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
	tunnel.serverIP = serverIP
	tunnel.serverIP4 = ip4Key(serverIP)

	pkt := []byte{0x45, 0, 0, 20}
	srcIP := net.IPv4(10, 0, 0, 2).To4()
	dstIP := net.IPv4(10, 0, 0, 99).To4() // unknown IP, not in peers

	go tunnel.routePacket(pkt, srcIP, dstIP)

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
	peer, ok := tunnel.peers[ip4Key(peerIP)]
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
	if peer.PublicAddr.String() != peerAddr.String() {
		t.Errorf("expected PublicAddr %s, got %s", peerAddr, peer.PublicAddr)
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
	tunnel.peers = map[[4]byte]*Peer{
		ip4Key(peerIP): {
			VirtualIP:  peerIP,
			PublicAddr: oldAddr,
			Username:   "player1",
		},
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
	peer, ok := tunnel.peers[ip4Key(peerIP)]
	tunnel.mu.RUnlock()

	if !ok {
		t.Fatal("expected peer to still be in peers map")
	}
	if peer.PublicAddr.String() != newAddr.String() {
		t.Errorf("expected PublicAddr %s, got %s", newAddr, peer.PublicAddr)
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
	tunnel.peers = map[[4]byte]*Peer{
		ip4Key(peer1IP): {
			VirtualIP:  peer1IP,
			PublicAddr: &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1000},
			Username:   "player1",
		},
		ip4Key(peer2IP): {
			VirtualIP:  peer2IP,
			PublicAddr: &net.UDPAddr{IP: net.IPv4(5, 6, 7, 8), Port: 2000},
			Username:   "player2",
		},
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
	_, hasPeer1 := tunnel.peers[ip4Key(peer1IP)]
	_, hasPeer2 := tunnel.peers[ip4Key(peer2IP)]
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
	tunnel.peers = map[[4]byte]*Peer{
		ip4Key(peerIP): {
			VirtualIP:  peerIP,
			PublicAddr: &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1000},
			Username:   "player1",
		},
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

	mock := &mockTunDevice{}
	tunnel.tunDev = mock

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
	tunnel.tunDev = mock

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
	tunnel.tunDev = nil // no TUN device

	dp := &protocol.DataPayload{
		SrcIP: net.IPv4(10, 0, 0, 1).To4(),
		DstIP: net.IPv4(10, 0, 0, 2).To4(),
		Data:  []byte{0x01, 0x02},
	}

	// Should not panic
	tunnel.handleDataFromServer(dp.Marshal())
}
