package server

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/holipay/gametunnel/internal/auth"
	"github.com/holipay/gametunnel/internal/protocol"
)

// ── Test Helpers ───────────────────────────────────────────────

func startTestServer(t *testing.T, password string, maxPlayers int) (*Server, *net.UDPAddr, context.CancelFunc) {
	t.Helper()
	_, subnet, _ := net.ParseCIDR("10.10.0.0/24")
	s, err := New(Config{
		Addr:       "127.0.0.1:0",
		Subnet:     subnet,
		MaxPlayers: maxPlayers,
		RoomPass:   password,
		Version:    "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	serverAddr := s.conn.LocalAddr().(*net.UDPAddr)
	return s, serverAddr, cancel
}

func sendAndRead(t *testing.T, conn *net.UDPConn, addr *net.UDPAddr, data []byte) *protocol.Message {
	t.Helper()
	if _, err := conn.WriteToUDP(data, addr); err != nil {
		t.Fatalf("send: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1500)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	msg, err := protocol.DecodeChecked(buf[:n])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return msg
}

func readMessage(t *testing.T, conn *net.UDPConn) *protocol.Message {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1500)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	msg, err := protocol.DecodeChecked(buf[:n])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return msg
}

func registerClient(t *testing.T, conn *net.UDPConn, serverAddr *net.UDPAddr, username, roomID string) *protocol.AssignIPPayload {
	t.Helper()
	reg := &protocol.RegisterPayload{RoomID: roomID, Username: username}
	packet := protocol.EncodeChecked(protocol.TypeRegister, reg.Marshal())
	msg := sendAndRead(t, conn, serverAddr, packet)
	if msg.Type != protocol.TypeAssignIP {
		t.Fatalf("expected AssignIP, got type %d", msg.Type)
	}
	assign, err := protocol.UnmarshalAssignIP(msg.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return assign
}

func newRegisteredClient(t *testing.T, serverAddr *net.UDPAddr, username, roomID string) (*net.UDPConn, net.IP) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	assign := registerClient(t, conn, serverAddr, username, roomID)
	return conn, assign.VirtualIP
}

// drainPeerInfo reads and discards any pending PeerInfo messages from the connection.
// Uses a short timeout so it returns quickly when there are no more pending packets.
func drainPeerInfo(t *testing.T, conn *net.UDPConn) {
	t.Helper()
	for {
		conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		buf := make([]byte, 1500)
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return // timeout or error — done draining
		}
		msg, err := protocol.DecodeChecked(buf[:n])
		if err != nil {
			continue
		}
		if msg.Type != protocol.TypePeerInfo {
			t.Fatalf("expected PeerInfo during drain, got type %d", msg.Type)
		}
	}
}

// ── Integration Tests ─────────────────────────────────────────

func TestIntegration_RegisterNoAuth(t *testing.T) {
	s, serverAddr, cancel := startTestServer(t, "", 4)
	defer cancel()
	defer s.Close()

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	assign := registerClient(t, conn, serverAddr, "player1", "room1")

	if !assign.VirtualIP.Equal(net.IPv4(10, 10, 0, 2)) {
		t.Fatalf("expected VirtualIP 10.10.0.2, got %s", assign.VirtualIP)
	}
	if !assign.ServerIP.Equal(net.IPv4(10, 10, 0, 1)) {
		t.Fatalf("expected ServerIP 10.10.0.1, got %s", assign.ServerIP)
	}
}

func TestIntegration_RegisterWithAuth(t *testing.T) {
	s, serverAddr, cancel := startTestServer(t, "test123", 4)
	defer cancel()
	defer s.Close()

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Step 1: Send Register, expect AuthChallenge
	reg := &protocol.RegisterPayload{RoomID: "room1", Username: "player1"}
	packet := protocol.EncodeChecked(protocol.TypeRegister, reg.Marshal())
	msg := sendAndRead(t, conn, serverAddr, packet)

	if msg.Type != protocol.TypeAuthChallenge {
		t.Fatalf("expected AuthChallenge, got type %d", msg.Type)
	}

	challenge, err := protocol.UnmarshalAuthChallenge(msg.Payload)
	if err != nil {
		t.Fatal(err)
	}

	// Step 2: Compute HMAC
	key := auth.DeriveKey("test123", "room1")
	clientAddr := conn.LocalAddr().(*net.UDPAddr)
	hmacVal := auth.ComputeHMAC(key, challenge.Challenge, "room1", "player1", clientAddr)

	// Step 3: Send AuthResponse, expect AssignIP
	authResp := &protocol.AuthResponsePayload{
		RoomID:   "room1",
		Username: "player1",
		HMAC:     hmacVal,
	}
	packet = protocol.EncodeChecked(protocol.TypeAuthResponse, authResp.Marshal())
	msg = sendAndRead(t, conn, serverAddr, packet)

	if msg.Type != protocol.TypeAssignIP {
		t.Fatalf("expected AssignIP, got type %d", msg.Type)
	}

	assign, err := protocol.UnmarshalAssignIP(msg.Payload)
	if err != nil {
		t.Fatal(err)
	}

	if !assign.VirtualIP.Equal(net.IPv4(10, 10, 0, 2)) {
		t.Fatalf("expected VirtualIP 10.10.0.2, got %s", assign.VirtualIP)
	}
}

func TestIntegration_DisconnectReclaimsIP(t *testing.T) {
	s, serverAddr, cancel := startTestServer(t, "", 4)
	defer cancel()
	defer s.Close()

	// Register client 1 → should get .2
	conn1, vip1 := newRegisteredClient(t, serverAddr, "player1", "room1")
	defer conn1.Close()
	drainPeerInfo(t, conn1)

	if !vip1.Equal(net.IPv4(10, 10, 0, 2)) {
		t.Fatalf("expected player1 VIP 10.10.0.2, got %s", vip1)
	}

	// Register client 2 → should get .3
	conn2, vip2 := newRegisteredClient(t, serverAddr, "player2", "room1")
	defer conn2.Close()
	drainPeerInfo(t, conn2)

	if !vip2.Equal(net.IPv4(10, 10, 0, 3)) {
		t.Fatalf("expected player2 VIP 10.10.0.3, got %s", vip2)
	}

	// Drain PeerInfo from client 1 (from client 2's registration)
	drainPeerInfo(t, conn1)

	// Client 1 sends Disconnect
	disc := protocol.EncodeChecked(protocol.TypeDisconnect, nil)
	if _, err := conn1.WriteToUDP(disc, serverAddr); err != nil {
		t.Fatalf("disconnect send: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// Drain PeerInfo from client 2 (from the disconnect notification)
	drainPeerInfo(t, conn2)

	// Register client 3 → should get .2 (reclaimed)
	conn3, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn3.Close()

	assign := registerClient(t, conn3, serverAddr, "player3", "room1")
	if !assign.VirtualIP.Equal(net.IPv4(10, 10, 0, 2)) {
		t.Fatalf("expected reclaimed VIP 10.10.0.2, got %s", assign.VirtualIP)
	}
}

func TestIntegration_UnicastRelay(t *testing.T) {
	s, serverAddr, cancel := startTestServer(t, "", 4)
	defer cancel()
	defer s.Close()

	// Register client A (.2) and client B (.3)
	connA, vipA := newRegisteredClient(t, serverAddr, "playerA", "room1")
	defer connA.Close()
	drainPeerInfo(t, connA)

	connB, vipB := newRegisteredClient(t, serverAddr, "playerB", "room1")
	defer connB.Close()
	drainPeerInfo(t, connB)

	// Drain PeerInfo from A (from B's registration)
	drainPeerInfo(t, connA)

	// Client A sends Data to client B
	dp := &protocol.DataPayload{SrcIP: vipA, DstIP: vipB, Data: []byte("hello")}
	packet := protocol.EncodeChecked(protocol.TypeData, dp.Marshal())
	if _, err := connA.WriteToUDP(packet, serverAddr); err != nil {
		t.Fatalf("send data: %v", err)
	}

	// Client B reads the relayed Data
	msg := readMessage(t, connB)
	if msg.Type != protocol.TypeData {
		t.Fatalf("expected Data, got type %d", msg.Type)
	}

	data, err := protocol.UnmarshalData(msg.Payload)
	if err != nil {
		t.Fatal(err)
	}

	if string(data.Data) != "hello" {
		t.Fatalf("expected payload 'hello', got '%s'", string(data.Data))
	}
	if !data.SrcIP.Equal(vipA) {
		t.Fatalf("expected SrcIP %s, got %s", vipA, data.SrcIP)
	}
	if !data.DstIP.Equal(vipB) {
		t.Fatalf("expected DstIP %s, got %s", vipB, data.DstIP)
	}
}

func TestIntegration_BroadcastRelay(t *testing.T) {
	s, serverAddr, cancel := startTestServer(t, "", 4)
	defer cancel()
	defer s.Close()

	// Register clients A (.2), B (.3), C (.4)
	connA, vipA := newRegisteredClient(t, serverAddr, "playerA", "room1")
	defer connA.Close()
	drainPeerInfo(t, connA)

	connB, _ := newRegisteredClient(t, serverAddr, "playerB", "room1")
	defer connB.Close()
	drainPeerInfo(t, connB)

	connC, _ := newRegisteredClient(t, serverAddr, "playerC", "room1")
	defer connC.Close()
	drainPeerInfo(t, connC)

	// Drain PeerInfo from A and B (from subsequent registrations)
	drainPeerInfo(t, connA)
	drainPeerInfo(t, connB)

	// Client A sends broadcast Data
	broadcastIP := net.IPv4(255, 255, 255, 255)
	dp := &protocol.DataPayload{SrcIP: vipA, DstIP: broadcastIP, Data: []byte("broadcast")}
	packet := protocol.EncodeChecked(protocol.TypeData, dp.Marshal())
	if _, err := connA.WriteToUDP(packet, serverAddr); err != nil {
		t.Fatalf("send broadcast: %v", err)
	}

	// Client B reads the broadcast
	msgB := readMessage(t, connB)
	if msgB.Type != protocol.TypeData {
		t.Fatalf("B: expected Data, got type %d", msgB.Type)
	}
	dataB, err := protocol.UnmarshalData(msgB.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if string(dataB.Data) != "broadcast" {
		t.Fatalf("B: expected payload 'broadcast', got '%s'", string(dataB.Data))
	}

	// Client C reads the broadcast
	msgC := readMessage(t, connC)
	if msgC.Type != protocol.TypeData {
		t.Fatalf("C: expected Data, got type %d", msgC.Type)
	}
	dataC, err := protocol.UnmarshalData(msgC.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if string(dataC.Data) != "broadcast" {
		t.Fatalf("C: expected payload 'broadcast', got '%s'", string(dataC.Data))
	}
}

func TestIntegration_IPExhaustion(t *testing.T) {
	s, serverAddr, cancel := startTestServer(t, "", 2)
	defer cancel()
	defer s.Close()

	// Register 2 clients to fill the room (maxPlayers=2)
	conn1, _ := newRegisteredClient(t, serverAddr, "player1", "room1")
	defer conn1.Close()
	drainPeerInfo(t, conn1)

	conn2, _ := newRegisteredClient(t, serverAddr, "player2", "room1")
	defer conn2.Close()
	drainPeerInfo(t, conn2)

	// Drain PeerInfo from client 1 (from client 2's registration)
	drainPeerInfo(t, conn1)

	// 3rd client tries to register — should be kicked
	conn3, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn3.Close()

	reg := &protocol.RegisterPayload{RoomID: "room1", Username: "player3"}
	packet := protocol.EncodeChecked(protocol.TypeRegister, reg.Marshal())
	msg := sendAndRead(t, conn3, serverAddr, packet)

	if msg.Type != protocol.TypeKick {
		t.Fatalf("expected Kick, got type %d", msg.Type)
	}

	kick, err := protocol.UnmarshalKick(msg.Payload)
	if err != nil {
		t.Fatal(err)
	}

	if kick.Reason == "" {
		t.Fatal("expected non-empty kick reason")
	}
}
