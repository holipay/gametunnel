// Package hostconfig loads host mode configuration from INI files.
package hostconfig

import (
	"os"
	"path/filepath"
	"strconv"

	"github.com/holipay/gametunnel/internal/iniconfig"
	"github.com/holipay/gametunnel/internal/paths"
)

// HostConfig holds configuration for the host mode (server + client combined).
type HostConfig struct {
	Addr       string
	Subnet     string
	MaxPlayers int
	RoomPass   string
	TCPAddr    string
	Verbose    bool
	PlayerName string
	RoomID     string
	Lang       string
}

// DefaultHostConfig returns a HostConfig with default values.
func DefaultHostConfig() *HostConfig {
	hostname, _ := os.Hostname()
	return &HostConfig{
		Addr:       ":4700",
		Subnet:     "10.10.0.0/24",
		MaxPlayers: 10,
		RoomID:     "default",
		PlayerName: hostname,
		Lang:       "zh",
	}
}

// PortableConfigPath returns the path to host.ini next to the exe.
func PortableConfigPath() string {
	return filepath.Join(paths.ExeDir(), "host.ini")
}

// LoadHostConfig loads host config from disk.
// Priority: host.ini next to exe > defaults.
func LoadHostConfig() *HostConfig {
	cfg := DefaultHostConfig()
	loadHostINI(PortableConfigPath(), cfg)
	return cfg
}

// loadHostINI parses a key=value config file into cfg. Returns true if file exists.
func loadHostINI(path string, cfg *HostConfig) bool {
	m, ok := iniconfig.ParseFile(path)
	if !ok {
		return false
	}
	if v := m["addr"]; v != "" {
		cfg.Addr = v
	}
	if v := m["subnet"]; v != "" {
		cfg.Subnet = v
	}
	if v := m["max"]; v != "" {
		if max, err := strconv.Atoi(v); err == nil && max > 0 {
			cfg.MaxPlayers = max
		}
	}
	if v := m["password"]; v != "" {
		cfg.RoomPass = v
	}
	if v := m["tcp-addr"]; v != "" {
		cfg.TCPAddr = v
	}
	if v := m["verbose"]; v != "" {
		cfg.Verbose = v == "true" || v == "1"
	}
	if v := m["name"]; v != "" {
		cfg.PlayerName = v
	}
	if v := m["room"]; v != "" {
		cfg.RoomID = v
	}
	if v := m["lang"]; v != "" {
		cfg.Lang = v
	}
	cfg.Addr = iniconfig.CombinePort(cfg.Addr, m["port"])
	return true
}
