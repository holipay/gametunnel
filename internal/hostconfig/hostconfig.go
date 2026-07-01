// Package hostconfig loads host mode configuration from INI files.
package hostconfig

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

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
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var portOnly string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		switch key {
		case "addr":
			if value != "" {
				cfg.Addr = value
			}
		case "subnet":
			if value != "" {
				cfg.Subnet = value
			}
		case "max":
			var v int
			if _, err := fmt.Sscanf(value, "%d", &v); err == nil && v > 0 {
				cfg.MaxPlayers = v
			}
		case "password":
			cfg.RoomPass = value
		case "tcp-addr":
			cfg.TCPAddr = value
		case "verbose":
			cfg.Verbose = value == "true" || value == "1"
		case "name":
			if value != "" {
				cfg.PlayerName = value
			}
		case "room":
			if value != "" {
				cfg.RoomID = value
			}
		case "lang":
			if value != "" {
				cfg.Lang = value
			}
		case "port":
			portOnly = value
		}
	}
	// Combine addr and port if both are specified separately
	if cfg.Addr != "" && portOnly != "" {
		host, _, err := net.SplitHostPort(cfg.Addr)
		if err != nil {
			// Addr has no port yet — append it
			addr := cfg.Addr
			if strings.HasPrefix(addr, "[") && strings.HasSuffix(addr, "]") {
				addr = addr[1 : len(addr)-1]
			}
			cfg.Addr = net.JoinHostPort(addr, portOnly)
		} else {
			_ = host
		}
	}
	return true
}
