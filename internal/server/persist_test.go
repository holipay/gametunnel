package server

import (
	"github.com/holipay/gametunnel/internal/netkey"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ── loadState Tests ───────────────────────────────────────────

func TestLoadState_NoStateDir(t *testing.T) {
	s := &Server{stateDir: ""}
	if err := s.loadState(); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestLoadState_NoDefaultRoom(t *testing.T) {
	s := &Server{stateDir: t.TempDir()}
	if err := s.loadState(); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestLoadState_FileNotExist(t *testing.T) {
	dir := t.TempDir()
	conn, _ := net.ListenUDP("udp", &net.UDPAddr{})
	defer conn.Close()
	room, _ := NewRoom(RoomConfig{
		RoomID:     "default",
		Subnet:     &net.IPNet{IP: net.IPv4(10, 10, 0, 0), Mask: net.CIDRMask(24, 32)},
		MaxPlayers: 10,
		Conn:       conn,
	})
	s := &Server{stateDir: dir, defaultRoom: room}
	if err := s.loadState(); err != nil {
		t.Errorf("expected nil for missing file, got %v", err)
	}
}

func TestLoadState_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, stateFileName), []byte("not json"), 0644)

	conn, _ := net.ListenUDP("udp", &net.UDPAddr{})
	defer conn.Close()
	room, _ := NewRoom(RoomConfig{
		RoomID:     "default",
		Subnet:     &net.IPNet{IP: net.IPv4(10, 10, 0, 0), Mask: net.CIDRMask(24, 32)},
		MaxPlayers: 10,
		Conn:       conn,
	})
	s := &Server{stateDir: dir, defaultRoom: room}
	if err := s.loadState(); err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestLoadState_VersionMismatch(t *testing.T) {
	dir := t.TempDir()
	state := RoomState{
		Version:   999, // wrong version
		Subnet:    "10.10.0.0/24",
		UpdatedAt: time.Now(),
		Clients:   map[string]ClientEntry{},
	}
	data, _ := json.Marshal(state)
	os.WriteFile(filepath.Join(dir, stateFileName), data, 0644)

	conn, _ := net.ListenUDP("udp", &net.UDPAddr{})
	defer conn.Close()
	room, _ := NewRoom(RoomConfig{
		RoomID:     "default",
		Subnet:     &net.IPNet{IP: net.IPv4(10, 10, 0, 0), Mask: net.CIDRMask(24, 32)},
		MaxPlayers: 10,
		Conn:       conn,
	})
	s := &Server{stateDir: dir, defaultRoom: room}
	if err := s.loadState(); err != nil {
		t.Errorf("version mismatch should return nil, got %v", err)
	}
	// No clients should be restored
	room.mu.RLock()
	n := len(room.clients)
	room.mu.RUnlock()
	if n != 0 {
		t.Errorf("expected 0 clients, got %d", n)
	}
}

func TestLoadState_SubnetMismatch(t *testing.T) {
	dir := t.TempDir()
	state := RoomState{
		Version:   stateVersion,
		Subnet:    "10.20.0.0/24", // wrong subnet
		UpdatedAt: time.Now(),
		Clients:   map[string]ClientEntry{},
	}
	data, _ := json.Marshal(state)
	os.WriteFile(filepath.Join(dir, stateFileName), data, 0644)

	conn, _ := net.ListenUDP("udp", &net.UDPAddr{})
	defer conn.Close()
	room, _ := NewRoom(RoomConfig{
		RoomID:     "default",
		Subnet:     &net.IPNet{IP: net.IPv4(10, 10, 0, 0), Mask: net.CIDRMask(24, 32)},
		MaxPlayers: 10,
		Conn:       conn,
	})
	s := &Server{stateDir: dir, defaultRoom: room}
	if err := s.loadState(); err != nil {
		t.Errorf("subnet mismatch should return nil, got %v", err)
	}
}

func TestLoadState_RestoreClient(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	state := RoomState{
		Version:   stateVersion,
		Subnet:    "10.10.0.0/24",
		UpdatedAt: now,
		Clients: map[string]ClientEntry{
			"10.10.0.2": {
				Username:  "alice",
				VirtualIP: "10.10.0.2",
				LastSeen:  now.Add(-5 * time.Second), // recent
			},
		},
	}
	data, _ := json.Marshal(state)
	os.WriteFile(filepath.Join(dir, stateFileName), data, 0644)

	conn, _ := net.ListenUDP("udp", &net.UDPAddr{})
	defer conn.Close()
	room, _ := NewRoom(RoomConfig{
		RoomID:     "default",
		Subnet:     &net.IPNet{IP: net.IPv4(10, 10, 0, 0), Mask: net.CIDRMask(24, 32)},
		MaxPlayers: 10,
		Conn:       conn,
	})
	s := &Server{stateDir: dir, defaultRoom: room}
	if err := s.loadState(); err != nil {
		t.Fatalf("loadState: %v", err)
	}

	room.mu.RLock()
	c := room.clients[netkey.IPKey(net.IPv4(10, 10, 0, 2))]
	room.mu.RUnlock()

	if c == nil {
		t.Fatal("expected client to be restored")
	}
	if c.Username != "alice" {
		t.Errorf("username: got %q, want %q", c.Username, "alice")
	}
	if !c.VirtualIP.Equal(net.IPv4(10, 10, 0, 2)) {
		t.Errorf("virtualIP: got %v, want 10.10.0.2", c.VirtualIP)
	}
	if c.PublicAddr != nil {
		t.Error("PublicAddr should be nil until reconnect")
	}
}

func TestLoadState_SkipStaleClient(t *testing.T) {
	dir := t.TempDir()
	state := RoomState{
		Version:   stateVersion,
		Subnet:    "10.10.0.0/24",
		UpdatedAt: time.Now().Add(-2 * time.Minute), // outside grace period
		Clients: map[string]ClientEntry{
			"10.10.0.2": {
				Username:  "stale",
				VirtualIP: "10.10.0.2",
				LastSeen:  time.Now().Add(-1 * time.Minute), // stale (> 30s)
			},
		},
	}
	data, _ := json.Marshal(state)
	os.WriteFile(filepath.Join(dir, stateFileName), data, 0644)

	conn, _ := net.ListenUDP("udp", &net.UDPAddr{})
	defer conn.Close()
	room, _ := NewRoom(RoomConfig{
		RoomID:     "default",
		Subnet:     &net.IPNet{IP: net.IPv4(10, 10, 0, 0), Mask: net.CIDRMask(24, 32)},
		MaxPlayers: 10,
		Conn:       conn,
	})
	s := &Server{stateDir: dir, defaultRoom: room}
	if err := s.loadState(); err != nil {
		t.Fatalf("loadState: %v", err)
	}

	room.mu.RLock()
	n := len(room.clients)
	room.mu.RUnlock()
	if n != 0 {
		t.Errorf("stale client should be skipped, got %d clients", n)
	}
}

func TestLoadState_RestoreStaleWithinGrace(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	state := RoomState{
		Version:   stateVersion,
		Subnet:    "10.10.0.0/24",
		UpdatedAt: now.Add(-10 * time.Second), // within grace period
		Clients: map[string]ClientEntry{
			"10.10.0.2": {
				Username:  "stale-but-grace",
				VirtualIP: "10.10.0.2",
				LastSeen:  now.Add(-1 * time.Minute), // stale
			},
		},
	}
	data, _ := json.Marshal(state)
	os.WriteFile(filepath.Join(dir, stateFileName), data, 0644)

	conn, _ := net.ListenUDP("udp", &net.UDPAddr{})
	defer conn.Close()
	room, _ := NewRoom(RoomConfig{
		RoomID:     "default",
		Subnet:     &net.IPNet{IP: net.IPv4(10, 10, 0, 0), Mask: net.CIDRMask(24, 32)},
		MaxPlayers: 10,
		Conn:       conn,
	})
	s := &Server{stateDir: dir, defaultRoom: room}
	if err := s.loadState(); err != nil {
		t.Fatalf("loadState: %v", err)
	}

	room.mu.RLock()
	c := room.clients[netkey.IPKey(net.IPv4(10, 10, 0, 2))]
	room.mu.RUnlock()
	if c == nil {
		t.Error("stale client within grace period should be restored")
	}
}

func TestLoadState_SkipInvalidIP(t *testing.T) {
	dir := t.TempDir()
	state := RoomState{
		Version:   stateVersion,
		Subnet:    "10.10.0.0/24",
		UpdatedAt: time.Now(),
		Clients: map[string]ClientEntry{
			"not-an-ip": {
				Username:  "bad",
				VirtualIP: "not-an-ip",
				LastSeen:  time.Now(),
			},
		},
	}
	data, _ := json.Marshal(state)
	os.WriteFile(filepath.Join(dir, stateFileName), data, 0644)

	conn, _ := net.ListenUDP("udp", &net.UDPAddr{})
	defer conn.Close()
	room, _ := NewRoom(RoomConfig{
		RoomID:     "default",
		Subnet:     &net.IPNet{IP: net.IPv4(10, 10, 0, 0), Mask: net.CIDRMask(24, 32)},
		MaxPlayers: 10,
		Conn:       conn,
	})
	s := &Server{stateDir: dir, defaultRoom: room}
	if err := s.loadState(); err != nil {
		t.Fatalf("loadState: %v", err)
	}

	room.mu.RLock()
	n := len(room.clients)
	room.mu.RUnlock()
	if n != 0 {
		t.Errorf("invalid IP should be skipped, got %d clients", n)
	}
}

func TestLoadState_SkipOutOfRangeOctet(t *testing.T) {
	dir := t.TempDir()
	state := RoomState{
		Version:   stateVersion,
		Subnet:    "10.10.0.0/24",
		UpdatedAt: time.Now(),
		Clients: map[string]ClientEntry{
			"10.10.0.1": { // .1 is server IP, octet < 2
				Username:  "server-ip",
				VirtualIP: "10.10.0.1",
				LastSeen:  time.Now(),
			},
			"10.10.0.255": { // .255 is broadcast, octet >= 255
				Username:  "broadcast",
				VirtualIP: "10.10.0.255",
				LastSeen:  time.Now(),
			},
		},
	}
	data, _ := json.Marshal(state)
	os.WriteFile(filepath.Join(dir, stateFileName), data, 0644)

	conn, _ := net.ListenUDP("udp", &net.UDPAddr{})
	defer conn.Close()
	room, _ := NewRoom(RoomConfig{
		RoomID:     "default",
		Subnet:     &net.IPNet{IP: net.IPv4(10, 10, 0, 0), Mask: net.CIDRMask(24, 32)},
		MaxPlayers: 10,
		Conn:       conn,
	})
	s := &Server{stateDir: dir, defaultRoom: room}
	if err := s.loadState(); err != nil {
		t.Fatalf("loadState: %v", err)
	}

	room.mu.RLock()
	n := len(room.clients)
	room.mu.RUnlock()
	if n != 0 {
		t.Errorf("out-of-range octets should be skipped, got %d clients", n)
	}
}

// ── saveState Tests ──────────────────────────────────────────

func TestSaveState_NoStateDir(t *testing.T) {
	s := &Server{stateDir: ""}
	// Should not panic
	s.saveState()
}

func TestSaveState_NoDefaultRoom(t *testing.T) {
	s := &Server{stateDir: t.TempDir()}
	// Should not panic
	s.saveState()
}

func TestSaveState_WritesValidJSON(t *testing.T) {
	dir := t.TempDir()
	conn, _ := net.ListenUDP("udp", &net.UDPAddr{})
	defer conn.Close()
	room, _ := NewRoom(RoomConfig{
		RoomID:     "default",
		Subnet:     &net.IPNet{IP: net.IPv4(10, 10, 0, 0), Mask: net.CIDRMask(24, 32)},
		MaxPlayers: 10,
		Conn:       conn,
	})

	// Add a client
	c := &Client{
		Username:  "bob",
		VirtualIP: net.IPv4(10, 10, 0, 2),
	}
	c.SetLastSeen(time.Now())
	room.mu.Lock()
	room.clients[netkey.IPKey(c.VirtualIP)] = c
	room.markIPUsed(c.VirtualIP)
	room.mu.Unlock()

	s := &Server{stateDir: dir, defaultRoom: room}
	s.saveState()

	// Read and parse the file
	data, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}

	var state RoomState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("parse state file: %v", err)
	}

	if state.Version != stateVersion {
		t.Errorf("version: got %d, want %d", state.Version, stateVersion)
	}
	if state.Subnet != "10.10.0.0/24" {
		t.Errorf("subnet: got %q, want %q", state.Subnet, "10.10.0.0/24")
	}
	if len(state.Clients) != 1 {
		t.Fatalf("clients: got %d, want 1", len(state.Clients))
	}
	entry := state.Clients["10.10.0.2"]
	if entry.Username != "bob" {
		t.Errorf("username: got %q, want %q", entry.Username, "bob")
	}
}

func TestSaveState_NoTmpFileLeftBehind(t *testing.T) {
	dir := t.TempDir()
	conn, _ := net.ListenUDP("udp", &net.UDPAddr{})
	defer conn.Close()
	room, _ := NewRoom(RoomConfig{
		RoomID:     "default",
		Subnet:     &net.IPNet{IP: net.IPv4(10, 10, 0, 0), Mask: net.CIDRMask(24, 32)},
		MaxPlayers: 10,
		Conn:       conn,
	})
	s := &Server{stateDir: dir, defaultRoom: room}
	s.saveState()

	// .tmp file should not exist after atomic rename
	tmpPath := filepath.Join(dir, stateFileName+".tmp")
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Error("tmp file should not exist after save")
	}
}

// ── Round-trip Test ──────────────────────────────────────────

func TestPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	conn, _ := net.ListenUDP("udp", &net.UDPAddr{})
	defer conn.Close()
	_, subnet, _ := net.ParseCIDR("10.10.0.0/24")
	room, _ := NewRoom(RoomConfig{
		RoomID:     "default",
		Subnet:     subnet,
		MaxPlayers: 10,
		Conn:       conn,
	})

	// Add two clients
	now := time.Now()
	for i, name := range []string{"alice", "bob"} {
		ip := net.IPv4(10, 10, 0, byte(i+2))
		c := &Client{Username: name, VirtualIP: ip}
		c.SetLastSeen(now)
		room.mu.Lock()
		room.clients[netkey.IPKey(ip)] = c
		room.markIPUsed(ip)
		room.mu.Unlock()
	}

	// Save
	s := &Server{stateDir: dir, defaultRoom: room}
	s.saveState()

	// Create fresh room and load
	room2, _ := NewRoom(RoomConfig{
		RoomID:     "default",
		Subnet:     subnet,
		MaxPlayers: 10,
		Conn:       conn,
	})
	s2 := &Server{stateDir: dir, defaultRoom: room2}
	if err := s2.loadState(); err != nil {
		t.Fatalf("loadState: %v", err)
	}

	room2.mu.RLock()
	defer room2.mu.RUnlock()

	if len(room2.clients) != 2 {
		t.Fatalf("expected 2 clients, got %d", len(room2.clients))
	}

	for _, name := range []string{"alice", "bob"} {
		found := false
		for _, c := range room2.clients {
			if c.Username == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("client %q not found after round-trip", name)
		}
	}
}
