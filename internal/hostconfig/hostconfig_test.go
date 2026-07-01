package hostconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultHostConfig(t *testing.T) {
	cfg := DefaultHostConfig()
	if cfg.Addr != ":4700" {
		t.Errorf("default addr = %q, want :4700", cfg.Addr)
	}
	if cfg.Subnet != "10.10.0.0/24" {
		t.Errorf("default subnet = %q, want 10.10.0.0/24", cfg.Subnet)
	}
	if cfg.MaxPlayers != 10 {
		t.Errorf("default max = %d, want 10", cfg.MaxPlayers)
	}
	if cfg.RoomID != "default" {
		t.Errorf("default room = %q, want default", cfg.RoomID)
	}
	if cfg.Lang != "zh" {
		t.Errorf("default lang = %q, want zh", cfg.Lang)
	}
	if cfg.Verbose {
		t.Error("default verbose should be false")
	}
}

func TestLoadHostINI_AllFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "host.ini")
	content := `addr=:5000
subnet=10.20.0.0/24
max=20
password=secret
tcp-addr=:5001
verbose=true
name=TestHost
room=myroom
lang=en
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultHostConfig()
	if !loadHostINI(path, cfg) {
		t.Fatal("loadHostINI returned false")
	}

	if cfg.Addr != ":5000" {
		t.Errorf("addr = %q, want :5000", cfg.Addr)
	}
	if cfg.Subnet != "10.20.0.0/24" {
		t.Errorf("subnet = %q, want 10.20.0.0/24", cfg.Subnet)
	}
	if cfg.MaxPlayers != 20 {
		t.Errorf("max = %d, want 20", cfg.MaxPlayers)
	}
	if cfg.RoomPass != "secret" {
		t.Errorf("password = %q, want secret", cfg.RoomPass)
	}
	if cfg.TCPAddr != ":5001" {
		t.Errorf("tcp-addr = %q, want :5001", cfg.TCPAddr)
	}
	if !cfg.Verbose {
		t.Error("verbose = false, want true")
	}
	if cfg.PlayerName != "TestHost" {
		t.Errorf("name = %q, want TestHost", cfg.PlayerName)
	}
	if cfg.RoomID != "myroom" {
		t.Errorf("room = %q, want myroom", cfg.RoomID)
	}
	if cfg.Lang != "en" {
		t.Errorf("lang = %q, want en", cfg.Lang)
	}
}

func TestLoadHostINI_CommentsAndBlanks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "host.ini")
	content := `# This is a comment
addr=:5000

# Another comment
subnet=10.20.0.0/24
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultHostConfig()
	loadHostINI(path, cfg)

	if cfg.Addr != ":5000" {
		t.Errorf("addr = %q, want :5000", cfg.Addr)
	}
	if cfg.Subnet != "10.20.0.0/24" {
		t.Errorf("subnet = %q, want 10.20.0.0/24", cfg.Subnet)
	}
	// Other fields should keep defaults
	if cfg.MaxPlayers != 10 {
		t.Errorf("max = %d, want 10 (default)", cfg.MaxPlayers)
	}
}

func TestLoadHostINI_NotFound(t *testing.T) {
	cfg := DefaultHostConfig()
	if loadHostINI("/nonexistent/path/host.ini", cfg) {
		t.Error("loadHostINI should return false for missing file")
	}
	// Defaults should be unchanged
	if cfg.Addr != ":4700" {
		t.Errorf("addr = %q, want :4700 (default)", cfg.Addr)
	}
}

func TestLoadHostINI_EmptyValuesPreserveDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "host.ini")
	content := `name=
room=
lang=
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultHostConfig()
	loadHostINI(path, cfg)

	// Empty values should not override defaults
	if cfg.PlayerName == "" {
		t.Error("name should preserve default, got empty")
	}
	if cfg.RoomID != "default" {
		t.Errorf("room = %q, want default (default preserved)", cfg.RoomID)
	}
	if cfg.Lang != "zh" {
		t.Errorf("lang = %q, want zh (default preserved)", cfg.Lang)
	}
}

func TestLoadHostINI_SeparateAddrPort(t *testing.T) {
	// When addr already has a port, separate port= is ignored (same as client config)
	dir := t.TempDir()
	path := filepath.Join(dir, "host.ini")
	content := `addr=:5000
port=8080
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultHostConfig()
	loadHostINI(path, cfg)

	if cfg.Addr != ":5000" {
		t.Errorf("addr = %q, want :5000 (addr already has port, port= ignored)", cfg.Addr)
	}
}

func TestLoadHostINI_PortOnly(t *testing.T) {
	// When addr has no port, port= is appended
	dir := t.TempDir()
	path := filepath.Join(dir, "host.ini")
	content := `addr=0.0.0.0
port=8080
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultHostConfig()
	loadHostINI(path, cfg)

	if cfg.Addr != "0.0.0.0:8080" {
		t.Errorf("addr = %q, want 0.0.0.0:8080", cfg.Addr)
	}
}

func TestLoadHostINI_VerboseValues(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"true", true},
		{"1", true},
		{"false", false},
		{"0", false},
		{"yes", false}, // only "true" and "1" are accepted
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "host.ini")
			content := "verbose=" + tt.value + "\n"
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}

			cfg := DefaultHostConfig()
			loadHostINI(path, cfg)

			if cfg.Verbose != tt.want {
				t.Errorf("verbose=%q: got %v, want %v", tt.value, cfg.Verbose, tt.want)
			}
		})
	}
}

func TestLoadHostINI_MaxPlayersInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "host.ini")
	content := `max=abc
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultHostConfig()
	loadHostINI(path, cfg)

	// Invalid max should keep default
	if cfg.MaxPlayers != 10 {
		t.Errorf("max = %d, want 10 (default preserved on invalid)", cfg.MaxPlayers)
	}
}

func TestLoadHostINI_MaxPlayersZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "host.ini")
	content := `max=0
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultHostConfig()
	loadHostINI(path, cfg)

	// Zero max should keep default (validation: v > 0)
	if cfg.MaxPlayers != 10 {
		t.Errorf("max = %d, want 10 (default preserved for zero)", cfg.MaxPlayers)
	}
}
