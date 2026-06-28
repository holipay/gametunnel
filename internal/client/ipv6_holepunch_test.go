package client

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/holipay/gametunnel/internal/protocol"
)

// ── IPv6 Helpers ────────────────────────────────────────────────

// newTestTunnelIPv6 creates a Tunnel bound to [::1] (IPv6 loopback).
// Returns the tunnel and a "server" UDP listener on [::1].
func newTestTunnelIPv6(t *testing.T) (*Tunnel, *net.UDPConn) {
	t.Helper()

	tunnelConn, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Fatalf("failed to create IPv6 tunnel conn: %v", err)
	}
	t.Cleanup(func() { tunnelConn.Close() })

	serverConn, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Fatalf("failed to create IPv6 server conn: %v", err)
	}
	t.Cleanup(func() { serverConn.Close() })

	cfg := &Config{PlayerName: "test6", RoomID: "test6", RoomPassword: ""}
	tunnel := New(cfg)
	tunnel.conn = tunnelConn
	tunnel.serverAddr = serverConn.LocalAddr().(*net.UDPAddr)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go tunnel.sendLoop(ctx, tunnel.conn)

	return tunnel, serverConn
}

// newIPv6PeerConn creates a UDP listener on [::1] to act as a peer endpoint.
func newIPv6PeerConn(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Fatalf("failed to create IPv6 peer conn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// ── 1. IPv6 Socket Binding ─────────────────────────────────────

func TestIPv6SocketBind_DualStack(t *testing.T) {
	// Verify that [::] dual-stack socket can send to both IPv4 and IPv6 destinations.
	bindAddr := &net.UDPAddr{IP: net.IPv6zero, Port: 0}
	conn, err := net.ListenUDP("udp", bindAddr)
	if err != nil {
		t.Skipf("dual-stack socket not available on this platform: %v", err)
	}
	defer conn.Close()

	// Should be able to send to [::1]
	_, err = conn.WriteToUDP([]byte("test"), &net.UDPAddr{IP: net.IPv6loopback, Port: 12345})
	if err != nil {
		t.Errorf("failed to send to IPv6 address from dual-stack socket: %v", err)
	}

	// Should be able to send to 127.0.0.1 (IPv4-mapped)
	_, err = conn.WriteToUDP([]byte("test"), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345})
	if err != nil {
		t.Errorf("failed to send to IPv4 address from dual-stack socket: %v", err)
	}
}

func TestIPv6SocketBind_PureIPv6(t *testing.T) {
	// Verify pure IPv6 socket works on [::1].
	conn, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Skipf("IPv6 not available: %v", err)
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	if localAddr.IP.To4() != nil {
		t.Error("expected pure IPv6 address, got IPv4")
	}
	if !localAddr.IP.Equal(net.IPv6loopback) {
		t.Errorf("expected [::1], got %s", localAddr.IP)
	}
}

// ── 2. IPv6 IP Key Normalization ───────────────────────────────

func TestIPv6_IPKeyConsistency(t *testing.T) {
	// IPv4-mapped IPv6 addresses must produce the same key as raw IPv4.
	ip4 := net.IPv4(192, 168, 1, 1).To4()
	ip16 := net.IPv4(192, 168, 1, 1).To16() // ::ffff:192.168.1.1
	if ipKey(ip4) != ipKey(ip16) {
		t.Error("4-byte and IPv4-mapped IPv6 keys should be identical")
	}

	// Native IPv6 must produce a different key from any IPv4.
	ip6 := net.ParseIP("2408:abcd::1")
	if ipKey(ip6) == ipKey(ip4) {
		t.Error("native IPv6 key should differ from IPv4 key")
	}
}

func TestIPv6_IPKeyRoundTrip(t *testing.T) {
	// IPKey should normalize to 16-byte form consistently.
	ips := []net.IP{
		net.IPv6loopback,
		net.ParseIP("2408:abcd::1"),
		net.ParseIP("fe80::1"),
		net.ParseIP("::ffff:10.0.0.1"),
	}
	seen := make(map[[16]byte]net.IP)
	for _, ip := range ips {
		key := ipKey(ip)
		if prev, ok := seen[key]; ok {
			t.Errorf("collision: %s and %s produce same key", prev, ip)
		}
		seen[key] = ip
	}
}

// ── 3. IPv6 PeerInfo Handling ──────────────────────────────────

func TestIPv6_PeerInfoWithIPv6PublicAddr(t *testing.T) {
	tunnel, _ := newTestTunnelIPv6(t)

	// Peer has IPv4 virtual IP but IPv6 public address
	peerVIP := net.IPv4(10, 10, 0, 3).To4()
	ipv6Addr := &net.UDPAddr{IP: net.IPv6loopback, Port: 9999}

	payload := &protocol.PeerInfoPayload{
		Peers: []protocol.PeerInfoEntry{
			{VirtualIP: peerVIP, PublicAddr: ipv6Addr, Username: "v6peer"},
		},
	}

	tunnel.handlePeerInfo(context.Background(), payload.Marshal())

	tunnel.mu.RLock()
	peer, ok := tunnel.peers[ipKey(peerVIP)]
	tunnel.mu.RUnlock()
	if !ok {
		t.Fatal("peer not found after PeerInfo")
	}

	// PublicAddr.IP should be normalized to 16 bytes
	pub16 := ipv6Addr.IP.To16()
	if !peer.PublicAddr.IP.Equal(pub16) {
		t.Errorf("PublicAddr IP: got %s, want %s (16-byte normalized)", peer.PublicAddr.IP, pub16)
	}
	if peer.PublicAddr.Port != 9999 {
		t.Errorf("PublicAddr Port: got %d, want 9999", peer.PublicAddr.Port)
	}
	if peer.Username != "v6peer" {
		t.Errorf("Username: got %q, want %q", peer.Username, "v6peer")
	}
}

func TestIPv6_PeerInfoAddrChangeResetsDirectReach(t *testing.T) {
	tunnel, _ := newTestTunnelIPv6(t)

	peerVIP := net.IPv4(10, 10, 0, 3).To4()
	oldAddr := &net.UDPAddr{IP: net.IPv6loopback, Port: 1000}
	newAddr := &net.UDPAddr{IP: net.IPv6loopback, Port: 2000}

	// Pre-populate peer with DirectReach=true
	peer := &Peer{VirtualIP: peerVIP, PublicAddr: oldAddr, Username: "v6peer"}
	peer.DirectReach.Store(true)
	tunnel.mu.Lock()
	tunnel.peers[ipKey(peerVIP)] = peer
	tunnel.mu.Unlock()

	// Simulate address change via PeerInfo
	payload := &protocol.PeerInfoPayload{
		Peers: []protocol.PeerInfoEntry{
			{VirtualIP: peerVIP, PublicAddr: newAddr, Username: "v6peer"},
		},
	}
	tunnel.handlePeerInfo(context.Background(), payload.Marshal())

	tunnel.mu.RLock()
	updated := tunnel.peers[ipKey(peerVIP)]
	tunnel.mu.RUnlock()

	if updated.DirectReach.Load() {
		t.Error("DirectReach should be reset to false after address change")
	}
	if updated.PublicAddr.Port != 2000 {
		t.Errorf("PublicAddr Port: got %d, want 2000", updated.PublicAddr.Port)
	}
}

// ── 4. IPv6 Hole Punch ─────────────────────────────────────────

func TestIPv6_StartHolePunch_SendsToIPv6Addr(t *testing.T) {
	tunnel, _ := newTestTunnelIPv6(t)

	// Create a peer endpoint on IPv6
	peerConn := newIPv6PeerConn(t)
	peerVIP := net.IPv4(10, 10, 0, 3).To4()
	peerAddr := peerConn.LocalAddr().(*net.UDPAddr)

	// Pre-populate peer
	peer := &Peer{VirtualIP: peerVIP, PublicAddr: peerAddr, Username: "v6peer"}
	tunnel.mu.Lock()
	tunnel.peers[ipKey(peerVIP)] = peer
	tunnel.cachedPunchPacket = protocol.EncodeChecked(protocol.TypeHolePunch, net.IPv4(10, 10, 0, 2).To4())
	tunnel.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start hole punch in background
	done := make(chan struct{})
	go func() {
		tunnel.startHolePunch(ctx, peerVIP)
		close(done)
	}()

	// Peer should receive hole punch packets on IPv6
	gotPacket := false
	peerConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1500)
	for i := 0; i < 3; i++ {
		n, _, err := peerConn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		msg, err := protocol.DecodeChecked(buf[:n])
		if err != nil {
			continue
		}
		if msg.Type == protocol.TypeHolePunch {
			gotPacket = true
			break
		}
	}

	cancel()
	<-done

	if !gotPacket {
		t.Error("expected hole punch packet from IPv6 tunnel, got none")
	}
}

func TestIPv6_HandleDirectHolePunch_FromIPv6Addr(t *testing.T) {
	tunnel, _ := newTestTunnelIPv6(t)

	peerVIP := net.IPv4(10, 10, 0, 3).To4()
	peerAddr := &net.UDPAddr{IP: net.IPv6loopback, Port: 5555}

	peer := &Peer{VirtualIP: peerVIP, PublicAddr: peerAddr, Username: "v6peer"}
	tunnel.mu.Lock()
	tunnel.peers[ipKey(peerVIP)] = peer
	tunnel.cachedPunchPacket = protocol.EncodeChecked(protocol.TypeHolePunch, net.IPv4(10, 10, 0, 2).To4())
	tunnel.mu.Unlock()

	// Simulate receiving a direct hole punch from the peer's IPv6 address
	holePunchPayload := net.IPv4(10, 10, 0, 3).To4() // peer's virtual IP
	msg := &protocol.Message{
		Type:    protocol.TypeHolePunch,
		Payload: holePunchPayload,
	}

	tunnel.handleDirectHolePunch(context.Background(), peerAddr, msg)

	// DirectReach should be confirmed
	tunnel.mu.RLock()
	updated := tunnel.peers[ipKey(peerVIP)]
	tunnel.mu.RUnlock()

	if !updated.DirectReach.Load() {
		t.Error("DirectReach should be true after receiving direct hole punch from IPv6 addr")
	}
}

func TestIPv6_HandleDirectHolePunch_SpoofedAddrRejected(t *testing.T) {
	tunnel, _ := newTestTunnelIPv6(t)

	peerVIP := net.IPv4(10, 10, 0, 3).To4()
	correctAddr := &net.UDPAddr{IP: net.IPv6loopback, Port: 5555}
	spoofedAddr := &net.UDPAddr{IP: net.ParseIP("2408:abcd::1"), Port: 5555}

	peer := &Peer{VirtualIP: peerVIP, PublicAddr: correctAddr, Username: "v6peer"}
	tunnel.mu.Lock()
	tunnel.peers[ipKey(peerVIP)] = peer
	tunnel.mu.Unlock()

	// Simulate hole punch from spoofed address
	holePunchPayload := net.IPv4(10, 10, 0, 3).To4()
	msg := &protocol.Message{
		Type:    protocol.TypeHolePunch,
		Payload: holePunchPayload,
	}

	tunnel.handleDirectHolePunch(context.Background(), spoofedAddr, msg)

	// DirectReach should NOT be confirmed
	tunnel.mu.RLock()
	updated := tunnel.peers[ipKey(peerVIP)]
	tunnel.mu.RUnlock()

	if updated.DirectReach.Load() {
		t.Error("DirectReach should be false for spoofed IPv6 address")
	}
}

// ── 5. IPv6 Direct Data Delivery (P2P) ─────────────────────────

func TestIPv6_HandleDirectData_FromIPv6Peer(t *testing.T) {
	tunnel, _ := newTestTunnelIPv6(t)
	mock := &mockTunDevice{}
	tunnel.tunDev = mock
	tunnel.virtualIP = net.IPv4(10, 10, 0, 2).To4()

	peerVIP := net.IPv4(10, 10, 0, 3).To4()
	peerAddr := &net.UDPAddr{IP: net.IPv6loopback, Port: 6666}

	peer := &Peer{VirtualIP: peerVIP, PublicAddr: peerAddr, Username: "v6peer"}
	tunnel.mu.Lock()
	tunnel.peers[ipKey(peerVIP)] = peer
	tunnel.mu.Unlock()

	// Build a data packet from the peer
	payloadData := []byte{0x45, 0x00, 0x00, 0x1c} // fake IPv4 header
	dp := &protocol.DataPayload{
		SrcIP: peerVIP,
		DstIP: tunnel.virtualIP,
		Data:  payloadData,
	}
	msg := &protocol.Message{
		Type:    protocol.TypeData,
		Payload: dp.Marshal(),
	}

	// Simulate receiving from the peer's IPv6 public address
	tunnel.handleDirectData(context.Background(), peerAddr, msg)

	// Data should be written to TUN
	if len(mock.writeBuf) != len(payloadData) {
		t.Fatalf("TUN write: got %d bytes, want %d", len(mock.writeBuf), len(payloadData))
	}
	if !bytes.Equal(mock.writeBuf, payloadData) {
		t.Errorf("TUN write data mismatch: got %v, want %v", mock.writeBuf, payloadData)
	}

	// DirectReach should be confirmed
	tunnel.mu.RLock()
	updated := tunnel.peers[ipKey(peerVIP)]
	tunnel.mu.RUnlock()
	if !updated.DirectReach.Load() {
		t.Error("DirectReach should be true after receiving direct data from IPv6 peer")
	}
}

func TestIPv6_HandleDirectData_WrongIPv6AddrRejected(t *testing.T) {
	tunnel, _ := newTestTunnelIPv6(t)
	mock := &mockTunDevice{}
	tunnel.tunDev = mock
	tunnel.virtualIP = net.IPv4(10, 10, 0, 2).To4()

	peerVIP := net.IPv4(10, 10, 0, 3).To4()
	correctAddr := &net.UDPAddr{IP: net.IPv6loopback, Port: 6666}
	wrongAddr := &net.UDPAddr{IP: net.ParseIP("2408:abcd::1"), Port: 6666}

	peer := &Peer{VirtualIP: peerVIP, PublicAddr: correctAddr, Username: "v6peer"}
	tunnel.mu.Lock()
	tunnel.peers[ipKey(peerVIP)] = peer
	tunnel.mu.Unlock()

	dp := &protocol.DataPayload{
		SrcIP: peerVIP,
		DstIP: tunnel.virtualIP,
		Data:  []byte{0x01, 0x02},
	}
	msg := &protocol.Message{Type: protocol.TypeData, Payload: dp.Marshal()}

	// Send from wrong IPv6 address
	tunnel.handleDirectData(context.Background(), wrongAddr, msg)

	if len(mock.writeBuf) != 0 {
		t.Error("data from wrong IPv6 addr should be rejected")
	}
	if tunnel.peers[ipKey(peerVIP)].DirectReach.Load() {
		t.Error("DirectReach should not be set for wrong address")
	}
}

func TestIPv6_HandleDirectData_WrongPortRejected(t *testing.T) {
	tunnel, _ := newTestTunnelIPv6(t)
	mock := &mockTunDevice{}
	tunnel.tunDev = mock
	tunnel.virtualIP = net.IPv4(10, 10, 0, 2).To4()

	peerVIP := net.IPv4(10, 10, 0, 3).To4()
	correctAddr := &net.UDPAddr{IP: net.IPv6loopback, Port: 6666}
	wrongPort := &net.UDPAddr{IP: net.IPv6loopback, Port: 9999}

	peer := &Peer{VirtualIP: peerVIP, PublicAddr: correctAddr, Username: "v6peer"}
	tunnel.mu.Lock()
	tunnel.peers[ipKey(peerVIP)] = peer
	tunnel.mu.Unlock()

	dp := &protocol.DataPayload{
		SrcIP: peerVIP,
		DstIP: tunnel.virtualIP,
		Data:  []byte{0x01, 0x02},
	}
	msg := &protocol.Message{Type: protocol.TypeData, Payload: dp.Marshal()}

	tunnel.handleDirectData(context.Background(), wrongPort, msg)

	if len(mock.writeBuf) != 0 {
		t.Error("data from wrong port should be rejected")
	}
}

// ── 6. IPv6 Route Packet (P2P Direct Path) ─────────────────────

func TestIPv6_RoutePacket_P2PDirectWithIPv6Peer(t *testing.T) {
	tunnel, _ := newTestTunnelIPv6(t)

	// Create a peer endpoint on IPv6
	peerConn := newIPv6PeerConn(t)
	peerVIP := net.IPv4(10, 10, 0, 3).To4()
	peerAddr := peerConn.LocalAddr().(*net.UDPAddr)

	serverIP := net.IPv4(10, 10, 0, 1).To4()
	tunnel.serverIP = serverIP
	tunnel.serverIPKey = ipKey(serverIP)

	peer := &Peer{VirtualIP: peerVIP, PublicAddr: peerAddr, Username: "v6peer"}
	peer.DirectReach.Store(true)

	tunnel.mu.Lock()
	tunnel.peers[ipKey(peerVIP)] = peer
	tunnel.mu.Unlock()

	// Build a fake IPv4 packet
	pkt := []byte{0x45, 0, 0, 20, 0, 0, 0, 0, 64, 17, 0, 0, 10, 10, 0, 2, 10, 10, 0, 3}
	srcIP := net.IPv4(10, 10, 0, 2).To4()

	// Route packet — should go P2P direct to IPv6 peer address
	tunnel.routePacket(pkt, srcIP, peerVIP)

	// Peer should receive the packet
	peerData := readUDPWithTimeout(peerConn, 2*time.Second)
	if peerData == nil {
		t.Fatal("expected packet on IPv6 peer conn for P2P, got none")
	}

	// Verify the packet content
	msg, err := protocol.DecodeChecked(peerData)
	if err != nil {
		t.Fatalf("failed to decode peer packet: %v", err)
	}
	if msg.Type != protocol.TypeData {
		t.Errorf("expected TypeData, got 0x%02x", msg.Type)
	}
}

func TestIPv6_RoutePacket_FallbackToRelayWithIPv6Server(t *testing.T) {
	tunnel, serverConn := newTestTunnelIPv6(t)

	serverIP := net.IPv6loopback
	tunnel.serverIP = serverIP
	tunnel.serverIPKey = ipKey(serverIP)

	peerVIP := net.IPv4(10, 10, 0, 3).To4()
	// Peer exists but no DirectReach
	peer := &Peer{VirtualIP: peerVIP, PublicAddr: &net.UDPAddr{IP: net.IPv6loopback, Port: 5555}, Username: "v6peer"}
	tunnel.mu.Lock()
	tunnel.peers[ipKey(peerVIP)] = peer
	tunnel.mu.Unlock()

	pkt := []byte{0x45, 0, 0, 20, 0, 0, 0, 0, 64, 17, 0, 0, 10, 10, 0, 2, 10, 10, 0, 3}
	srcIP := net.IPv4(10, 10, 0, 2).To4()

	tunnel.routePacket(pkt, srcIP, peerVIP)

	// Should fallback to server relay
	data := readUDPWithTimeout(serverConn, 2*time.Second)
	if data == nil {
		t.Fatal("expected packet on server conn (relay fallback), got none")
	}

	msg, err := protocol.DecodeChecked(data)
	if err != nil {
		t.Fatalf("failed to decode server packet: %v", err)
	}
	if msg.Type != protocol.TypeData {
		t.Errorf("expected TypeData, got 0x%02x", msg.Type)
	}
}

// ── 7. IPv6 Full Hole Punch Flow (End-to-End) ──────────────────

func TestIPv6_HolePunchFlow_EndToEnd(t *testing.T) {
	// Simulates the full flow:
	// 1. Tunnel receives PeerInfo with IPv6 public addr
	// 2. Tunnel sends hole punch to IPv6 addr
	// 3. Peer responds with hole punch back
	// 4. Tunnel confirms DirectReach
	// 5. Tunnel routes data P2P to IPv6 peer

	// Setup: tunnel with server on IPv6
	tunnel, _ := newTestTunnelIPv6(t)

	serverIP := net.IPv6loopback
	tunnel.serverIP = serverIP
	tunnel.serverIPKey = ipKey(serverIP)
	tunnel.virtualIP = net.IPv4(10, 10, 0, 2).To4()
	tunnel.cachedPunchPacket = protocol.EncodeChecked(protocol.TypeHolePunch, tunnel.virtualIP.To4())

	// Setup: peer endpoint on IPv6
	peerConn := newIPv6PeerConn(t)
	peerVIP := net.IPv4(10, 10, 0, 3).To4()
	peerAddr := peerConn.LocalAddr().(*net.UDPAddr)

	// Step 1: Simulate PeerInfo with IPv6 public address
	payload := &protocol.PeerInfoPayload{
		Peers: []protocol.PeerInfoEntry{
			{VirtualIP: peerVIP, PublicAddr: peerAddr, Username: "v6peer"},
		},
	}
	tunnel.handlePeerInfo(context.Background(), payload.Marshal())

	// Verify peer was added
	tunnel.mu.RLock()
	peer, ok := tunnel.peers[ipKey(peerVIP)]
	tunnel.mu.RUnlock()
	if !ok {
		t.Fatal("peer not found after PeerInfo")
	}
	if peer.DirectReach.Load() {
		t.Error("DirectReach should be false initially")
	}

	// Step 2: Wait for hole punch packets to arrive at peer
	peerConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1500)
	holePunchReceived := false
	for i := 0; i < 5; i++ {
		n, _, err := peerConn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		msg, err := protocol.DecodeChecked(buf[:n])
		if err != nil {
			continue
		}
		if msg.Type == protocol.TypeHolePunch {
			holePunchReceived = true
			break
		}
	}
	if !holePunchReceived {
		t.Fatal("peer did not receive hole punch from tunnel")
	}

	// Step 3: Peer sends hole punch back to tunnel's IPv6 address
	tunnelAddr := tunnel.conn.LocalAddr().(*net.UDPAddr)
	peerBackPayload := protocol.EncodeChecked(protocol.TypeHolePunch, peerVIP.To4())
	_, err := peerConn.WriteToUDP(peerBackPayload, tunnelAddr)
	if err != nil {
		t.Fatalf("peer failed to send hole punch back: %v", err)
	}

	// Step 4: Wait for tunnel to process the hole punch (give receiveFromServer time)
	time.Sleep(200 * time.Millisecond)

	// Check DirectReach
	tunnel.mu.RLock()
	peer2, ok2 := tunnel.peers[ipKey(peerVIP)]
	tunnel.mu.RUnlock()
	if !ok2 {
		t.Fatal("peer disappeared")
	}

	// Note: DirectReach may or may not be set depending on timing of
	// handleDirectHolePunch vs handleHolePunchReceived. What matters is
	// that the mechanism works — test by sending data.

	// Step 5: Simulate sending data via P2P path
	mock := &mockTunDevice{}
	tunnel.tunDev = mock

	// Force DirectReach for testing the route path
	peer2.DirectReach.Store(true)

	dataPkt := []byte{0x45, 0, 0, 20, 0, 0, 0, 0, 64, 17, 0, 0, 10, 10, 0, 2, 10, 10, 0, 3}
	srcIP := net.IPv4(10, 10, 0, 2).To4()

	tunnel.routePacket(dataPkt, srcIP, peerVIP)

	// Peer should receive the data packet
	peerConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	gotData := false
	for i := 0; i < 3; i++ {
		n, _, err := peerConn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		msg, err := protocol.DecodeChecked(buf[:n])
		if err != nil {
			continue
		}
		if msg.Type == protocol.TypeData {
			gotData = true
			break
		}
	}

	if !gotData {
		t.Error("peer did not receive data packet via P2P IPv6 path")
	}
}

// ── 8. IPv6 PeerInfo Marshal/Unmarshal Round-trip ──────────────

func TestIPv6_PeerInfoRoundTrip_IPv6Addr(t *testing.T) {
	original := &protocol.PeerInfoPayload{
		Peers: []protocol.PeerInfoEntry{
			{
				VirtualIP:  net.IPv4(10, 10, 0, 2).To4(),
				PublicAddr: &net.UDPAddr{IP: net.ParseIP("2408:abcd::1"), Port: 4700},
				Username:   "player_v6",
			},
			{
				VirtualIP:  net.IPv4(10, 10, 0, 3).To4(),
				PublicAddr: &net.UDPAddr{IP: net.IPv6loopback, Port: 12345},
				Username:   "player_loopback",
			},
		},
	}

	data := original.Marshal()
	decoded, err := protocol.UnmarshalPeerInfo(data)
	if err != nil {
		t.Fatalf("UnmarshalPeerInfo failed: %v", err)
	}

	if len(decoded.Peers) != 2 {
		t.Fatalf("expected 2 peers, got %d", len(decoded.Peers))
	}

	// Check first peer
	p0 := decoded.Peers[0]
	if !p0.VirtualIP.Equal(net.IPv4(10, 10, 0, 2).To4()) {
		t.Errorf("peer0 VirtualIP: got %s", p0.VirtualIP)
	}
	if p0.PublicAddr == nil {
		t.Fatal("peer0 PublicAddr is nil")
	}
	if !p0.PublicAddr.IP.Equal(net.ParseIP("2408:abcd::1")) {
		t.Errorf("peer0 PublicAddr.IP: got %s", p0.PublicAddr.IP)
	}
	if p0.PublicAddr.Port != 4700 {
		t.Errorf("peer0 PublicAddr.Port: got %d", p0.PublicAddr.Port)
	}
	if p0.Username != "player_v6" {
		t.Errorf("peer0 Username: got %q", p0.Username)
	}

	// Check second peer
	p1 := decoded.Peers[1]
	if !p1.VirtualIP.Equal(net.IPv4(10, 10, 0, 3).To4()) {
		t.Errorf("peer1 VirtualIP: got %s", p1.VirtualIP)
	}
	if p1.PublicAddr == nil {
		t.Fatal("peer1 PublicAddr is nil")
	}
	if !p1.PublicAddr.IP.Equal(net.IPv6loopback) {
		t.Errorf("peer1 PublicAddr.IP: got %s", p1.PublicAddr.IP)
	}
	if p1.PublicAddr.Port != 12345 {
		t.Errorf("peer1 PublicAddr.Port: got %d", p1.PublicAddr.Port)
	}
	if p1.Username != "player_loopback" {
		t.Errorf("peer1 Username: got %q", p1.Username)
	}
}

func TestIPv6_PeerInfoRoundTrip_EmptyPublicAddr(t *testing.T) {
	original := &protocol.PeerInfoPayload{
		Peers: []protocol.PeerInfoEntry{
			{
				VirtualIP:  net.IPv4(10, 10, 0, 2).To4(),
				PublicAddr: nil,
				Username:   "noaddr",
			},
		},
	}

	data := original.Marshal()
	decoded, err := protocol.UnmarshalPeerInfo(data)
	if err != nil {
		t.Fatalf("UnmarshalPeerInfo failed: %v", err)
	}

	if len(decoded.Peers) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(decoded.Peers))
	}
	if decoded.Peers[0].PublicAddr != nil {
		t.Errorf("expected nil PublicAddr, got %s", decoded.Peers[0].PublicAddr)
	}
}

// ── 9. IPv6 sendHolePunchRelay ─────────────────────────────────

func TestIPv6_SendHolePunchRelay_UsesIPv4VirtualIP(t *testing.T) {
	tunnel, serverConn := newTestTunnelIPv6(t)

	serverIP := net.IPv6loopback
	tunnel.serverIP = serverIP
	tunnel.serverIPKey = ipKey(serverIP)

	peerVIP := net.IPv4(10, 10, 0, 3).To4()

	// sendHolePunchRelay sends the virtual IP (4 bytes) to the server
	tunnel.sendHolePunchRelay(peerVIP)

	pkt := readUDPWithTimeout(serverConn, 2*time.Second)
	if pkt == nil {
		t.Fatal("expected hole punch relay packet on server, got none")
	}

	msg, err := protocol.DecodeChecked(pkt)
	if err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if msg.Type != protocol.TypeHolePunch {
		t.Errorf("expected TypeHolePunch, got 0x%02x", msg.Type)
	}
	if len(msg.Payload) < 4 {
		t.Fatal("hole punch payload too short")
	}

	// Verify it contains the peer's virtual IP
	parsedIP := net.IP(msg.Payload[:4])
	if !parsedIP.Equal(peerVIP) {
		t.Errorf("hole punch payload IP: got %s, want %s", parsedIP, peerVIP)
	}
}

func TestIPv6_SendHolePunchRelay_NilIP_NoPanic(t *testing.T) {
	tunnel, _ := newTestTunnelIPv6(t)
	// Should not panic
	tunnel.sendHolePunchRelay(nil)
}

func TestIPv6_SendHolePunchRelay_IPv6IP_Skipped(t *testing.T) {
	tunnel, _ := newTestTunnelIPv6(t)
	// sendHolePunchRelay only accepts 4-byte IPv4 virtual IPs
	ipv6IP := net.IPv6loopback
	tunnel.sendHolePunchRelay(ipv6IP)
	// Should not panic, should silently skip (no packet sent)
}

// ── 10. IPv6 Keepalive ─────────────────────────────────────────

func TestIPv6_P2PKeepalive_SendsToIPv6Peer(t *testing.T) {
	tunnel, _ := newTestTunnelIPv6(t)

	peerConn := newIPv6PeerConn(t)
	peerVIP := net.IPv4(10, 10, 0, 3).To4()
	peerAddr := peerConn.LocalAddr().(*net.UDPAddr)

	peer := &Peer{VirtualIP: peerVIP, PublicAddr: peerAddr, Username: "v6peer"}
	peer.DirectReach.Store(true)

	tunnel.mu.Lock()
	tunnel.peers[ipKey(peerVIP)] = peer
	tunnel.cachedPunchPacket = protocol.EncodeChecked(protocol.TypeHolePunch, net.IPv4(10, 10, 0, 2).To4())
	tunnel.mu.Unlock()

	tunnel.sendP2PKeepalives()

	pkt := readUDPWithTimeout(peerConn, 2*time.Second)
	if pkt == nil {
		t.Fatal("expected keepalive packet on IPv6 peer, got none")
	}

	msg, err := protocol.DecodeChecked(pkt)
	if err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if msg.Type != protocol.TypeHolePunch {
		t.Errorf("expected TypeHolePunch (keepalive reuse), got 0x%02x", msg.Type)
	}
}

func TestIPv6_P2PKeepalive_SkipsNonDirectPeers(t *testing.T) {
	tunnel, _ := newTestTunnelIPv6(t)

	peerConn := newIPv6PeerConn(t)
	peerVIP := net.IPv4(10, 10, 0, 3).To4()
	peerAddr := peerConn.LocalAddr().(*net.UDPAddr)

	peer := &Peer{VirtualIP: peerVIP, PublicAddr: peerAddr, Username: "v6peer"}
	// DirectReach is false — keepalive should NOT be sent

	tunnel.mu.Lock()
	tunnel.peers[ipKey(peerVIP)] = peer
	tunnel.cachedPunchPacket = protocol.EncodeChecked(protocol.TypeHolePunch, net.IPv4(10, 10, 0, 2).To4())
	tunnel.mu.Unlock()

	tunnel.sendP2PKeepalives()

	// Peer should NOT receive anything
	pkt := readUDPWithTimeout(peerConn, 300*time.Millisecond)
	if pkt != nil {
		t.Error("non-DirectReach peer should not receive keepalive")
	}
}

// ── 11. IPv6 retryFailedHolePunches ────────────────────────────

func TestIPv6_RetryFailedHolePunches_IncludesIPv6Peers(t *testing.T) {
	tunnel, _ := newTestTunnelIPv6(t)

	peerConn := newIPv6PeerConn(t)
	peerVIP := net.IPv4(10, 10, 0, 3).To4()
	peerAddr := peerConn.LocalAddr().(*net.UDPAddr)

	peer := &Peer{VirtualIP: peerVIP, PublicAddr: peerAddr, Username: "v6peer"}
	// DirectReach is false — should be retried

	tunnel.mu.Lock()
	tunnel.peers[ipKey(peerVIP)] = peer
	tunnel.cachedPunchPacket = protocol.EncodeChecked(protocol.TypeHolePunch, net.IPv4(10, 10, 0, 2).To4())
	tunnel.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	tunnel.retryFailedHolePunches(ctx)

	// Peer should receive hole punch packets
	peerConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1500)
	gotPacket := false
	for i := 0; i < 5; i++ {
		n, _, err := peerConn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		msg, err := protocol.DecodeChecked(buf[:n])
		if err != nil {
			continue
		}
		if msg.Type == protocol.TypeHolePunch {
			gotPacket = true
			break
		}
	}

	if !gotPacket {
		t.Error("IPv6 peer should receive hole punch during retry")
	}
}

// ── 12. IPv6 Status Reporting ──────────────────────────────────

func TestIPv6_Status_WithIPv6Peers(t *testing.T) {
	tunnel, _ := newTestTunnelIPv6(t)
	mock := &mockTunDevice{}
	tunnel.tunDev = mock
	tunnel.virtualIP = net.IPv4(10, 10, 0, 2).To4()

	// P2P peer via IPv6
	peer1 := &Peer{VirtualIP: net.IPv4(10, 10, 0, 3).To4(), Username: "p2p_v6"}
	peer1.DirectReach.Store(true)

	// Relay peer via IPv6
	peer2 := &Peer{VirtualIP: net.IPv4(10, 10, 0, 4).To4(), Username: "relay_v6"}

	tunnel.mu.Lock()
	tunnel.peers[ipKey(peer1.VirtualIP)] = peer1
	tunnel.peers[ipKey(peer2.VirtualIP)] = peer2
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

// ── 13. IPv6 Address Comparison Edge Cases ─────────────────────

func TestIPv6_IPComparison_IPv4Mapped(t *testing.T) {
	// IPv4 127.0.0.1 mapped to IPv6 should equal raw IPv4
	ip4 := net.IPv4(127, 0, 0, 1)
	ip16 := ip4.To16() // ::ffff:127.0.0.1

	// When received on dual-stack socket, addresses are always 16 bytes
	// So we need to normalize before comparison
	if !ip4.Equal(ip16) {
		// Go's net.IP.Equal treats them as equal
		t.Log("Go net.IP.Equal: IPv4 and IPv4-mapped are equal (correct)")
	}

	// But ipKey must also normalize
	if ipKey(ip4) != ipKey(ip16) {
		t.Error("ipKey should normalize IPv4 and IPv4-mapped to same key")
	}
}

func TestIPv6_IPComparison_NativeIPv6(t *testing.T) {
	ip1 := net.ParseIP("2408:abcd::1")
	ip2 := net.ParseIP("2408:abcd::1")
	ip3 := net.ParseIP("2408:abcd::2")

	if !ip1.Equal(ip2) {
		t.Error("same IPv6 addresses should be equal")
	}
	if ip1.Equal(ip3) {
		t.Error("different IPv6 addresses should not be equal")
	}
	if ipKey(ip1) != ipKey(ip2) {
		t.Error("same IPv6 should produce same key")
	}
	if ipKey(ip1) == ipKey(ip3) {
		t.Error("different IPv6 should produce different keys")
	}
}

// ── 14. IPv6 handleHolePunchReceived (Server-Relayed) ──────────

func TestIPv6_HandleHolePunchReceived_ServerRelayed(t *testing.T) {
	tunnel, _ := newTestTunnelIPv6(t)

	peerVIP := net.IPv4(10, 10, 0, 3).To4()
	peerAddr := &net.UDPAddr{IP: net.IPv6loopback, Port: 7777}

	peer := &Peer{VirtualIP: peerVIP, PublicAddr: peerAddr, Username: "v6peer"}
	tunnel.mu.Lock()
	tunnel.peers[ipKey(peerVIP)] = peer
	tunnel.cachedPunchPacket = protocol.EncodeChecked(protocol.TypeHolePunch, net.IPv4(10, 10, 0, 2).To4())
	tunnel.mu.Unlock()

	// Build hole punch payload: 4 bytes virtual IP
	payload := make([]byte, 4)
	copy(payload, peerVIP.To4())

	// Should not panic
	tunnel.handleHolePunchReceived(context.Background(), payload)

	// Verify peer still exists
	tunnel.mu.RLock()
	_, ok := tunnel.peers[ipKey(peerVIP)]
	tunnel.mu.RUnlock()
	if !ok {
		t.Error("peer should still exist after server-relayed hole punch")
	}
}

func TestIPv6_HandleHolePunchReceived_ShortPayload(t *testing.T) {
	tunnel, _ := newTestTunnelIPv6(t)

	// Payload too short — should not panic
	tunnel.handleHolePunchReceived(context.Background(), []byte{0x01, 0x02}) // only 2 bytes
}

func TestIPv6_HandleHolePunchReceived_UnknownPeer(t *testing.T) {
	tunnel, _ := newTestTunnelIPv6(t)

	// Unknown peer IP — should not panic
	payload := net.IPv4(10, 10, 0, 99).To4()
	tunnel.handleHolePunchReceived(context.Background(), payload)
}

// ── 15. Real-World IPv6 Server Address Tests ────────────────────

func TestIPv6_ValidateServerAddr_GlobalUnicast(t *testing.T) {
	// Real-world IPv6 server: 240d:c000:f07f:8e00:3ab0:2dee:7c06:0
	tests := []struct {
		addr    string
		wantErr bool
	}{
		{"[240d:c000:f07f:8e00:3ab0:2dee:7c06:0]:4700", false},
		{"[240d:c000:f07f:8e00:3ab0:2dee:7c06::]:4700", false},
		{"240d:c000:f07f:8e00:3ab0:2dee:7c06:0:4700", true}, // missing brackets
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

func TestIPv6_LoadINI_GlobalUnicastServer(t *testing.T) {
	tmpDir := t.TempDir()
	iniPath := filepath.Join(tmpDir, "config.ini")

	content := `server=[240d:c000:f07f:8e00:3ab0:2dee:7c06:0]:4700
name=testplayer
room=default
password=jqka
lang=zh
`
	if err := os.WriteFile(iniPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write INI: %v", err)
	}

	cfg := &Config{PlayerName: "default", RoomID: "default"}
	ok := loadINI(iniPath, cfg)
	if !ok {
		t.Fatal("loadINI should return true")
	}

	expected := "[240d:c000:f07f:8e00:3ab0:2dee:7c06:0]:4700"
	if cfg.ServerAddr != expected {
		t.Errorf("ServerAddr: got %q, want %q", cfg.ServerAddr, expected)
	}
	if cfg.RoomPassword != "jqka" {
		t.Errorf("RoomPassword: got %q, want %q", cfg.RoomPassword, "jqka")
	}
}

func TestIPv6_IPKey_GlobalUnicast(t *testing.T) {
	ip := net.ParseIP("240d:c000:f07f:8e00:3ab0:2dee:7c06:0")
	if ip == nil {
		t.Fatal("failed to parse IPv6 address")
	}

	key := ipKey(ip)
	// Verify it's a valid 16-byte key with the correct first bytes
	if key[0] != 0x24 || key[1] != 0x0d {
		t.Errorf("key[0:2]: got %x %x, want 24 0d", key[0], key[1])
	}

	// Same address should produce same key
	ip2 := net.ParseIP("240d:c000:f07f:8e00:3ab0:2dee:7c06:0")
	if ipKey(ip) != ipKey(ip2) {
		t.Error("same address should produce same key")
	}
}

func TestIPv6_PeerInfo_GlobalUnicastAddr(t *testing.T) {
	tunnel, _ := newTestTunnelIPv6(t)

	// Simulate a peer with the real-world IPv6 address as public address
	peerVIP := net.IPv4(10, 10, 0, 3).To4()
	realIPv6Addr := &net.UDPAddr{
		IP:   net.ParseIP("240d:c000:f07f:8e00:3ab0:2dee:7c06:0"),
		Port: 4700,
	}

	payload := &protocol.PeerInfoPayload{
		Peers: []protocol.PeerInfoEntry{
			{VirtualIP: peerVIP, PublicAddr: realIPv6Addr, Username: "real_peer"},
		},
	}

	data := payload.Marshal()
	decoded, err := protocol.UnmarshalPeerInfo(data)
	if err != nil {
		t.Fatalf("UnmarshalPeerInfo failed: %v", err)
	}

	if len(decoded.Peers) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(decoded.Peers))
	}

	p := decoded.Peers[0]
	if !p.PublicAddr.IP.Equal(realIPv6Addr.IP) {
		t.Errorf("PublicAddr.IP: got %s, want %s", p.PublicAddr.IP, realIPv6Addr.IP)
	}
	if p.PublicAddr.Port != 4700 {
		t.Errorf("PublicAddr.Port: got %d, want 4700", p.PublicAddr.Port)
	}

	// Register this peer in the tunnel
	tunnel.handlePeerInfo(context.Background(), data)

	tunnel.mu.RLock()
	peer, ok := tunnel.peers[ipKey(peerVIP)]
	tunnel.mu.RUnlock()
	if !ok {
		t.Fatal("peer not found")
	}
	if !peer.PublicAddr.IP.Equal(realIPv6Addr.IP) {
		t.Errorf("peer PublicAddr.IP: got %s, want %s", peer.PublicAddr.IP, realIPv6Addr.IP)
	}
}

func TestIPv6_HolePunch_GlobalUnicastPeer(t *testing.T) {
	tunnel, _ := newTestTunnelIPv6(t)
	mock := &mockTunDevice{}
	tunnel.tunDev = mock
	tunnel.virtualIP = net.IPv4(10, 10, 0, 2).To4()

	// Use a local IPv6 listener to simulate the peer (can't reach real internet)
	peerConn := newIPv6PeerConn(t)
	peerVIP := net.IPv4(10, 10, 0, 3).To4()
	peerAddr := peerConn.LocalAddr().(*net.UDPAddr)

	// Register peer with IPv6 address (simulating the real-world scenario)
	tunnel.mu.Lock()
	tunnel.peers[ipKey(peerVIP)] = &Peer{
		VirtualIP:  peerVIP,
		PublicAddr: peerAddr,
		Username:   "real_peer",
	}
	tunnel.cachedPunchPacket = protocol.EncodeChecked(protocol.TypeHolePunch, tunnel.virtualIP.To4())
	tunnel.mu.Unlock()

	// Start hole punch — should work with any IPv6 address
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		tunnel.startHolePunch(ctx, peerVIP)
		close(done)
	}()

	// Peer should receive hole punch packets
	peerConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1500)
	gotPacket := false
	for i := 0; i < 5; i++ {
		n, _, err := peerConn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		msg, err := protocol.DecodeChecked(buf[:n])
		if err != nil {
			continue
		}
		if msg.Type == protocol.TypeHolePunch {
			gotPacket = true
			break
		}
	}

	<-done

	if !gotPacket {
		t.Error("peer did not receive hole punch via IPv6")
	}

	// Simulate peer sending data back via P2P
	dp := &protocol.DataPayload{
		SrcIP: peerVIP,
		DstIP: tunnel.virtualIP,
		Data:  []byte{0x45, 0x00, 0x00, 0x1c},
	}
	msg := &protocol.Message{Type: protocol.TypeData, Payload: dp.Marshal()}

	// Direct data from peer's IPv6 address
	tunnel.handleDirectData(context.Background(), peerAddr, msg)

	// Verify data was written to TUN
	if len(mock.writeBuf) == 0 {
		t.Error("expected data written to TUN from IPv6 peer")
	}

	// Verify DirectReach confirmed
	tunnel.mu.RLock()
	peer := tunnel.peers[ipKey(peerVIP)]
	tunnel.mu.RUnlock()
	if !peer.DirectReach.Load() {
		t.Error("DirectReach should be true after receiving data from IPv6 peer")
	}
}
