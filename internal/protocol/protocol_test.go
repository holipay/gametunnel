package protocol

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
)

// ── Wire Format: Encode/Decode Round-trip ───────────────────────

func TestEncodeDecodeRoundTrip(t *testing.T) {
	payloads := [][]byte{
		nil,
		[]byte{},
		[]byte("hello"),
		bytes.Repeat([]byte("x"), 1000),
	}
	for _, payload := range payloads {
		encoded := Encode(TypeKeepAlive, payload)
		msg, err := Decode(encoded)
		if err != nil {
			t.Fatalf("Decode failed for payload len=%d: %v", len(payload), err)
		}
		if msg.Type != TypeKeepAlive {
			t.Errorf("type: got %d, want %d", msg.Type, TypeKeepAlive)
		}
		if !bytes.Equal(msg.Payload, payload) {
			t.Errorf("payload mismatch: got %d bytes, want %d bytes", len(msg.Payload), len(payload))
		}
	}
}

func TestEncodeCheckedRoundTrip(t *testing.T) {
	payload := []byte("test data")
	encoded := EncodeChecked(TypeData, payload)
	msg, err := DecodeChecked(encoded)
	if err != nil {
		t.Fatalf("DecodeChecked failed: %v", err)
	}
	if msg.Type != TypeData {
		t.Errorf("type: got %d, want %d", msg.Type, TypeData)
	}
	if !bytes.Equal(msg.Payload, payload) {
		t.Errorf("payload mismatch")
	}
}

func TestVersionMismatch(t *testing.T) {
	encoded := Encode(TypeKeepAlive, nil)
	encoded[0] = 99
	_, err := Decode(encoded)
	if err == nil {
		t.Fatal("expected error for version mismatch")
	}
}

func TestChecksumCorruption(t *testing.T) {
	encoded := EncodeChecked(TypeData, []byte("hello"))
	encoded[len(encoded)-1] ^= 0xFF
	_, err := DecodeChecked(encoded)
	if err == nil {
		t.Fatal("expected checksum error for corrupted packet")
	}
}

func TestChecksumBodyCorruption(t *testing.T) {
	encoded := EncodeChecked(TypeData, []byte("hello"))
	encoded[3] ^= 0xFF
	_, err := DecodeChecked(encoded)
	if err == nil {
		t.Fatal("expected checksum error for corrupted body")
	}
}

func TestPacketTooShort(t *testing.T) {
	cases := [][]byte{nil, {}, {0x01}, {0x01, 0x02}}
	for _, c := range cases {
		_, err := DecodeChecked(c)
		if err == nil {
			t.Errorf("expected error for input len=%d", len(c))
		}
	}
}

// ── Register Marshal/Unmarshal ─────────────────────────────────

func TestRegisterRoundTrip(t *testing.T) {
	tests := []struct {
		roomID   string
		username string
	}{
		{"default", "Player1"},
		{"my-room", "测试用户"},
		{"", ""},
		{string(bytes.Repeat([]byte("a"), 32)), string(bytes.Repeat([]byte("b"), 32))},
	}
	for _, tt := range tests {
		r := &RegisterPayload{RoomID: tt.roomID, Username: tt.username}
		data := r.Marshal()
		r2, err := UnmarshalRegister(data)
		if err != nil {
			t.Fatalf("UnmarshalRegister failed: %v", err)
		}
		if r2.RoomID != tt.roomID {
			t.Errorf("roomID: got %q, want %q", r2.RoomID, tt.roomID)
		}
		if r2.Username != tt.username {
			t.Errorf("username: got %q, want %q", r2.Username, tt.username)
		}
	}
}

func TestRegisterTruncated(t *testing.T) {
	cases := [][]byte{
		nil,
		{0x01},
		{0x05, 0x00},
		{0x02, 0x00, 'a', 'b'},
		{0x02, 0x00, 'a', 'b', 0x03, 0x00},
	}
	for _, c := range cases {
		_, err := UnmarshalRegister(c)
		if err == nil {
			t.Errorf("expected error for input len=%d: %x", len(c), c)
		}
	}
}

// ── AssignIP Marshal/Unmarshal ─────────────────────────────────

func TestAssignIPRoundTrip(t *testing.T) {
	a := &AssignIPPayload{
		VirtualIP:  net.IPv4(10, 10, 0, 2).To4(),
		SubnetMask: net.CIDRMask(24, 32),
		ServerIP:   net.IPv4(10, 10, 0, 1).To4(),
	}
	data := a.Marshal()
	a2, err := UnmarshalAssignIP(data)
	if err != nil {
		t.Fatalf("UnmarshalAssignIP failed: %v", err)
	}
	if !a2.VirtualIP.Equal(a.VirtualIP) {
		t.Errorf("VirtualIP: got %v, want %v", a2.VirtualIP, a.VirtualIP)
	}
	if !a2.ServerIP.Equal(a.ServerIP) {
		t.Errorf("ServerIP: got %v, want %v", a2.ServerIP, a.ServerIP)
	}
	if !bytes.Equal([]byte(a2.SubnetMask), []byte(a.SubnetMask)) {
		t.Errorf("SubnetMask mismatch")
	}
}

func TestAssignIPTruncated(t *testing.T) {
	_, err := UnmarshalAssignIP([]byte{0x01, 0x02, 0x03})
	if err == nil {
		t.Fatal("expected error for short input")
	}
}

// ── Kick Marshal/Unmarshal ─────────────────────────────────────

func TestKickRoundTrip(t *testing.T) {
	reasons := []string{"房间已满", "密码错误", ""}
	for _, reason := range reasons {
		k := &KickPayload{Reason: reason}
		data := k.Marshal()
		k2, err := UnmarshalKick(data)
		if err != nil {
			t.Fatalf("UnmarshalKick failed: %v", err)
		}
		if k2.Reason != reason {
			t.Errorf("reason: got %q, want %q", k2.Reason, reason)
		}
	}
}

func TestKickTruncated(t *testing.T) {
	_, err := UnmarshalKick([]byte{0x05})
	if err == nil {
		t.Fatal("expected error for short input")
	}
}

// ── Data Marshal/Unmarshal ─────────────────────────────────────

func TestDataRoundTrip(t *testing.T) {
	d := &DataPayload{
		SrcIP: net.IPv4(10, 10, 0, 2).To4(),
		DstIP: net.IPv4(10, 10, 0, 3).To4(),
		Data:  []byte("game packet data"),
	}
	data := d.Marshal()
	d2, err := UnmarshalData(data)
	if err != nil {
		t.Fatalf("UnmarshalData failed: %v", err)
	}
	if !d2.SrcIP.Equal(d.SrcIP) {
		t.Errorf("SrcIP: got %v, want %v", d2.SrcIP, d.SrcIP)
	}
	if !d2.DstIP.Equal(d.DstIP) {
		t.Errorf("DstIP: got %v, want %v", d2.DstIP, d.DstIP)
	}
	if !bytes.Equal(d2.Data, d.Data) {
		t.Errorf("Data mismatch")
	}
}

func TestDataEmptyPayload(t *testing.T) {
	d := &DataPayload{
		SrcIP: net.IPv4(10, 10, 0, 2).To4(),
		DstIP: net.IPv4(10, 10, 0, 3).To4(),
		Data:  nil,
	}
	data := d.Marshal()
	d2, err := UnmarshalData(data)
	if err != nil {
		t.Fatalf("UnmarshalData failed: %v", err)
	}
	if len(d2.Data) != 0 {
		t.Errorf("expected empty data, got %d bytes", len(d2.Data))
	}
}

func TestDataTruncated(t *testing.T) {
	_, err := UnmarshalData([]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07})
	if err == nil {
		t.Fatal("expected error for short input")
	}
}

// ── PeerInfo Marshal/Unmarshal ─────────────────────────────────

func TestPeerInfoRoundTrip(t *testing.T) {
	p := &PeerInfoPayload{
		Peers: []PeerInfoEntry{
			{
				VirtualIP:  net.IPv4(10, 10, 0, 2).To4(),
				PublicAddr: &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 4700},
				Username:   "Alice",
			},
			{
				VirtualIP:  net.IPv4(10, 10, 0, 3).To4(),
				PublicAddr: &net.UDPAddr{IP: net.IPv4(5, 6, 7, 8), Port: 12345},
				Username:   "Bob",
			},
		},
	}
	data := p.Marshal()
	p2, err := UnmarshalPeerInfo(data)
	if err != nil {
		t.Fatalf("UnmarshalPeerInfo failed: %v", err)
	}
	if len(p2.Peers) != 2 {
		t.Fatalf("got %d peers, want 2", len(p2.Peers))
	}
	if !p2.Peers[0].VirtualIP.Equal(p.Peers[0].VirtualIP) {
		t.Errorf("peer 0 VirtualIP mismatch")
	}
	if p2.Peers[0].PublicAddr.String() != p.Peers[0].PublicAddr.String() {
		t.Errorf("peer 0 PublicAddr: got %v, want %v", p2.Peers[0].PublicAddr, p.Peers[0].PublicAddr)
	}
	if p2.Peers[0].Username != "Alice" {
		t.Errorf("peer 0 Username: got %q, want Alice", p2.Peers[0].Username)
	}
	if p2.Peers[1].Username != "Bob" {
		t.Errorf("peer 1 Username: got %q, want Bob", p2.Peers[1].Username)
	}
}

func TestPeerInfoEmpty(t *testing.T) {
	p := &PeerInfoPayload{}
	data := p.Marshal()
	p2, err := UnmarshalPeerInfo(data)
	if err != nil {
		t.Fatalf("UnmarshalPeerInfo failed: %v", err)
	}
	if len(p2.Peers) != 0 {
		t.Errorf("got %d peers, want 0", len(p2.Peers))
	}
}

func TestPeerInfoNilPeers(t *testing.T) {
	p := &PeerInfoPayload{Peers: nil}
	data := p.Marshal()
	p2, err := UnmarshalPeerInfo(data)
	if err != nil {
		t.Fatalf("UnmarshalPeerInfo failed: %v", err)
	}
	if len(p2.Peers) != 0 {
		t.Errorf("got %d peers, want 0", len(p2.Peers))
	}
}

// ── Auth Challenge/Response Marshal/Unmarshal ──────────────────

func TestAuthChallengeRoundTrip(t *testing.T) {
	challenge := make([]byte, 16)
	for i := range challenge {
		challenge[i] = byte(i)
	}
	a := &AuthChallengePayload{Challenge: challenge, ClientAddr: "114.219.29.15:50604"}
	data := a.Marshal()
	a2, err := UnmarshalAuthChallenge(data)
	if err != nil {
		t.Fatalf("UnmarshalAuthChallenge failed: %v", err)
	}
	if !bytes.Equal(a2.Challenge, challenge) {
		t.Errorf("challenge mismatch")
	}
	if a2.ClientAddr != "114.219.29.15:50604" {
		t.Errorf("ClientAddr: got %q, want %q", a2.ClientAddr, "114.219.29.15:50604")
	}
}

func TestAuthChallengeNoAddr(t *testing.T) {
	challenge := make([]byte, 16)
	a := &AuthChallengePayload{Challenge: challenge, ClientAddr: ""}
	data := a.Marshal()
	a2, err := UnmarshalAuthChallenge(data)
	if err != nil {
		t.Fatalf("UnmarshalAuthChallenge failed: %v", err)
	}
	if a2.ClientAddr != "" {
		t.Errorf("expected empty ClientAddr, got %q", a2.ClientAddr)
	}
}

func TestAuthChallengeTruncated(t *testing.T) {
	_, err := UnmarshalAuthChallenge([]byte{0x10})
	if err == nil {
		t.Fatal("expected error for short input")
	}
}

func TestAuthResponseRoundTrip(t *testing.T) {
	hmacVal := make([]byte, 32)
	for i := range hmacVal {
		hmacVal[i] = byte(i * 3)
	}
	a := &AuthResponsePayload{
		RoomID:   "myroom",
		Username: "Player1",
		HMAC:     hmacVal,
	}
	data := a.Marshal()
	a2, err := UnmarshalAuthResponse(data)
	if err != nil {
		t.Fatalf("UnmarshalAuthResponse failed: %v", err)
	}
	if a2.RoomID != "myroom" {
		t.Errorf("RoomID: got %q, want myroom", a2.RoomID)
	}
	if a2.Username != "Player1" {
		t.Errorf("Username: got %q, want Player1", a2.Username)
	}
	if !bytes.Equal(a2.HMAC, hmacVal) {
		t.Errorf("HMAC mismatch")
	}
}

// ── Message Type Constants ─────────────────────────────────────

func TestMessageTypeConstants(t *testing.T) {
	types := []struct {
		name string
		typ  byte
	}{
		{"Register", TypeRegister},
		{"AssignIP", TypeAssignIP},
		{"PeerInfo", TypePeerInfo},
		{"PeerRequest", TypePeerRequest},
		{"HolePunch", TypeHolePunch},
		{"Data", TypeData},
		{"KeepAlive", TypeKeepAlive},
		{"AuthChallenge", TypeAuthChallenge},
		{"AuthResponse", TypeAuthResponse},
		{"Kick", TypeKick},
		{"Disconnect", TypeDisconnect},
	}
	seen := make(map[byte]string)
	for _, tt := range types {
		if tt.typ == 0 {
			t.Errorf("%s: type should not be 0", tt.name)
		}
		if prev, ok := seen[tt.typ]; ok {
			t.Errorf("%s and %s have same type %d", prev, tt.name, tt.typ)
		}
		seen[tt.typ] = tt.name
	}
}

// ── IPv6 Multicast Detection ──────────────────────────────────

func TestIsIPv6Multicast(t *testing.T) {
	tests := []struct {
		name string
		ip   net.IP
		want bool
	}{
		{"IPv6 all-nodes ff02::1", net.ParseIP("ff02::1"), true},
		{"IPv6 mDNS ff02::fb", net.ParseIP("ff02::fb"), true},
		{"IPv6 solicited-node ff02::1:ff00:1", net.ParseIP("ff02::1:ff00:1"), true},
		{"IPv6 global unicast", net.ParseIP("2408:abcd::1"), false},
		{"IPv6 loopback", net.IPv6loopback, false},
		{"IPv4 multicast (not IPv6)", net.IPv4(224, 0, 0, 251), false},
		{"IPv4 broadcast", net.IPv4bcast, false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsIPv6Multicast(tt.ip); got != tt.want {
				t.Errorf("IsIPv6Multicast(%s) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
}

func TestIsRelayTarget_IPv6Multicast(t *testing.T) {
	_, subnet, _ := net.ParseCIDR("10.10.0.0/24")

	// IPv6 multicast should be a relay target
	if !IsRelayTarget(net.ParseIP("ff02::1"), subnet) {
		t.Error("ff02::1 should be a relay target")
	}

	// IPv6 unicast should NOT be a relay target
	if IsRelayTarget(net.ParseIP("2408:abcd::1"), subnet) {
		t.Error("2408:abcd::1 should not be a relay target")
	}
}

func TestPeerInfoWithIPv6PublicAddr(t *testing.T) {
	// Simulates the IPv6 transport scenario: virtual IP is IPv4,
	// but the peer's public address (from the server) is IPv6.
	p := &PeerInfoPayload{
		Peers: []PeerInfoEntry{
			{
				VirtualIP:  net.IPv4(10, 10, 0, 2).To4(),
				PublicAddr: &net.UDPAddr{IP: net.ParseIP("2408:abcd::1"), Port: 4700},
				Username:   "IPv6Player",
			},
		},
	}
	data := p.Marshal()
	p2, err := UnmarshalPeerInfo(data)
	if err != nil {
		t.Fatalf("UnmarshalPeerInfo failed: %v", err)
	}
	if len(p2.Peers) != 1 {
		t.Fatalf("got %d peers, want 1", len(p2.Peers))
	}
	if !p2.Peers[0].VirtualIP.Equal(net.IPv4(10, 10, 0, 2)) {
		t.Errorf("VirtualIP mismatch")
	}
	if p2.Peers[0].PublicAddr.IP.String() != "2408:abcd::1" {
		t.Errorf("PublicAddr IP: got %s, want 2408:abcd::1", p2.Peers[0].PublicAddr.IP)
	}
	if p2.Peers[0].PublicAddr.Port != 4700 {
		t.Errorf("PublicAddr Port: got %d, want 4700", p2.Peers[0].PublicAddr.Port)
	}
}

// ── Version Compatibility ─────────────────────────────────────

func TestIsCompatible(t *testing.T) {
	tests := []struct {
		name     string
		client   uint16
		server   uint16
		expected bool
	}{
		{"both zero (old versions)", 0, 0, true},
		{"client zero (old client)", 0, 0x0102, true},
		{"server zero (old server)", 0x0102, 0, true},
		{"same version", 0x0102, 0x0102, true},
		{"client older minor", 0x0101, 0x0102, true},
		{"client newer minor", 0x0103, 0x0102, false},
		{"different major", 0x0102, 0x0200, false},
		{"different major reverse", 0x0200, 0x0102, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsCompatible(tt.client, tt.server)
			if got != tt.expected {
				t.Errorf("IsCompatible(0x%04x, 0x%04x) = %v, want %v",
					tt.client, tt.server, got, tt.expected)
			}
		})
	}
}

func TestVersionMajorMinor(t *testing.T) {
	v := uint16(0x0102) // v1.2
	if major := VersionMajor(v); major != 1 {
		t.Errorf("VersionMajor(0x%04x) = %d, want 1", v, major)
	}
	if minor := VersionMinor(v); minor != 2 {
		t.Errorf("VersionMinor(0x%04x) = %d, want 2", v, minor)
	}
}

func TestRegisterVersionRoundTrip(t *testing.T) {
	r := &RegisterPayload{
		RoomID:   "test",
		Username: "player1",
		Version:  0x0102,
	}
	data := r.Marshal()
	r2, err := UnmarshalRegister(data)
	if err != nil {
		t.Fatalf("UnmarshalRegister failed: %v", err)
	}
	if r2.Version != 0x0102 {
		t.Errorf("Version: got 0x%04x, want 0x%04x", r2.Version, 0x0102)
	}
}

func TestRegisterBackwardCompatible(t *testing.T) {
	// Simulate old client that doesn't send version (shorter payload)
	roomBytes := []byte("test")
	userBytes := []byte("player1")
	buf := make([]byte, 2+len(roomBytes)+2+len(userBytes))
	off := 0
	binary.LittleEndian.PutUint16(buf[off:], uint16(len(roomBytes)))
	off += 2
	copy(buf[off:], roomBytes)
	off += len(roomBytes)
	binary.LittleEndian.PutUint16(buf[off:], uint16(len(userBytes)))
	off += 2
	copy(buf[off:], userBytes)

	r, err := UnmarshalRegister(buf)
	if err != nil {
		t.Fatalf("UnmarshalRegister failed: %v", err)
	}
	if r.Version != 0 {
		t.Errorf("Version: got %d, want 0 (old client)", r.Version)
	}
}

func TestAssignIPVersionRoundTrip(t *testing.T) {
	a := &AssignIPPayload{
		VirtualIP:  net.IPv4(10, 10, 0, 2),
		SubnetMask: net.CIDRMask(24, 32),
		ServerIP:   net.IPv4(10, 10, 0, 1),
		Version:    0x0102,
	}
	data := a.Marshal()
	a2, err := UnmarshalAssignIP(data)
	if err != nil {
		t.Fatalf("UnmarshalAssignIP failed: %v", err)
	}
	if a2.Version != 0x0102 {
		t.Errorf("Version: got 0x%04x, want 0x%04x", a2.Version, 0x0102)
	}
}

func TestAssignIPBackwardCompatible(t *testing.T) {
	// Simulate old server that doesn't send version (12 bytes)
	buf := make([]byte, 12)
	copy(buf[0:4], net.IPv4(10, 10, 0, 2).To4())
	copy(buf[4:8], net.CIDRMask(24, 32))
	copy(buf[8:12], net.IPv4(10, 10, 0, 1).To4())

	a, err := UnmarshalAssignIP(buf)
	if err != nil {
		t.Fatalf("UnmarshalAssignIP failed: %v", err)
	}
	if a.Version != 0 {
		t.Errorf("Version: got %d, want 0 (old server)", a.Version)
	}
}
