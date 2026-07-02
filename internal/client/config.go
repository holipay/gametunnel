package client

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/iniconfig"
	"github.com/holipay/gametunnel/internal/paths"
	"github.com/holipay/gametunnel/internal/tun"
)

// DefaultMTU is the tunnel MTU if not configured.
const DefaultMTU = tun.DefaultMTU

// MinMTU/MaxMTU define the allowed MTU range.
const (
	MinMTU = 576
	MaxMTU = 9000
)

// Config holds client configuration.
type Config struct {
	ServerAddr   string
	PlayerName   string
	RoomID       string
	RoomPassword string
	Lang         string // "zh" or "en", default "zh"
	MTU          int    // tunnel MTU, default DefaultMTU
	LogFile      string // log file path, empty = stderr only
	Verbose      bool   // enable verbose logging
}

// DefaultConfig returns a Config with default values.
func DefaultConfig() *Config {
	hostname, _ := os.Hostname()
	return &Config{
		PlayerName: hostname,
		RoomID:     "default",
		Lang:       "zh",
		MTU: DefaultMTU,
	}
}

// Validate checks the config for errors.
func (c *Config) Validate() error {
	if err := ValidateServerAddr(c.ServerAddr); err != nil {
		return err
	}
	if len(c.RoomID) == 0 || len(c.RoomID) > 32 {
		return fmt.Errorf("room ID must be 1-32 characters")
	}
	if c.MTU < MinMTU || c.MTU > MaxMTU {
		return fmt.Errorf("MTU must be %d-%d, got %d", MinMTU, MaxMTU, c.MTU)
	}
	return nil
}

// PortableConfigPath returns the path to config.ini next to the exe.
func PortableConfigPath() string {
	return filepath.Join(paths.ExeDir(), "config.ini")
}

// LegacyConfigPath returns the path to the legacy JSON config file.
func LegacyConfigPath() string {
	return filepath.Join(paths.AppDataDir(), "GameTunnel", "config.json")
}

// LoadConfig loads the config from disk.
// Priority: config.ini next to exe > AppData/config.json > defaults.
func LoadConfig() *Config {
	cfg := DefaultConfig()

	// Try portable config.ini first
	if loadINI(PortableConfigPath(), cfg) {
		return cfg
	}

	// Fall back to legacy JSON config
	loadJSON(LegacyConfigPath(), cfg)
	return cfg
}

// ValidateServerAddr checks that the server address is in host:port format.
// Returns a user-friendly error if invalid.
//
// Supported formats:
//   - IPv4:      1.2.3.4:4700
//   - IPv6:      [2408::1]:4700  (brackets required)
//   - Domain:    example.com:4700
func ValidateServerAddr(addr string) error {
	if addr == "" {
		return fmt.Errorf("server address is empty")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// IPv6 address without brackets — user likely forgot them
		if strings.Count(addr, ":") >= 2 && !strings.Contains(addr, "[") {
			return fmt.Errorf("IPv6 address needs brackets: [%s]:4700", addr)
		}
		// IPv6 with brackets but missing port
		if strings.HasPrefix(addr, "[") && strings.Contains(addr, "]") && !strings.Contains(addr, "]:") {
			return fmt.Errorf("IPv6 address missing port: use %s:4700", addr)
		}
		// Missing port — suggest adding :4700
		if !strings.Contains(addr, ":") {
			return fmt.Errorf("server address %q missing port (use %s:4700)", addr, addr)
		}
		return fmt.Errorf("invalid server address %q: %v", addr, err)
	}
	if host == "" {
		return fmt.Errorf("server address has empty host")
	}
	if port == "" {
		return fmt.Errorf("server address has empty port")
	}
	// Validate port is numeric and in valid range (1-65535)
	portNum := 0
	for _, c := range port {
		if c < '0' || c > '9' {
			return fmt.Errorf("invalid port %q in server address", port)
		}
		portNum = portNum*10 + int(c-'0')
		if portNum > 65535 {
			return fmt.Errorf("port out of range (1-65535)")
		}
	}
	if portNum < 1 {
		return fmt.Errorf("port %d out of range (1-65535)", portNum)
	}
	return nil
}

// SaveConfig writes the config to config.ini next to the exe.
func SaveConfig(cfg *Config) error {
	path := PortableConfigPath()
	t := i18n.T()
	var b strings.Builder
	fmt.Fprintln(&b, t.CfgHeader)
	fmt.Fprintln(&b, t.CfgServerHint)
	// Split host and port for cleaner IPv6 display
	host, port, err := net.SplitHostPort(cfg.ServerAddr)
	if err == nil {
		fmt.Fprintf(&b, "server=%s\n", host)
		fmt.Fprintf(&b, "port=%s\n", port)
	} else {
		fmt.Fprintf(&b, "server=%s\n", cfg.ServerAddr)
	}
	fmt.Fprintln(&b, t.CfgNameHint)
	fmt.Fprintf(&b, "name=%s\n", cfg.PlayerName)
	fmt.Fprintln(&b, t.CfgRoomHint)
	fmt.Fprintf(&b, "room=%s\n", cfg.RoomID)
	fmt.Fprintln(&b, t.CfgPassHint)
	fmt.Fprintf(&b, "password=%s\n", cfg.RoomPassword)
	fmt.Fprintln(&b, "# Language (zh or en)")
	fmt.Fprintf(&b, "lang=%s\n", cfg.Lang)
	fmt.Fprintln(&b, "# Tunnel MTU (576-9000, default 1400)")
	fmt.Fprintf(&b, "mtu=%d\n", cfg.MTU)
	fmt.Fprintln(&b, "# Log file path (empty = stderr only)")
	fmt.Fprintf(&b, "log-file=%s\n", cfg.LogFile)
	fmt.Fprintln(&b, "# Verbose logging (true / false)")
	fmt.Fprintf(&b, "verbose=%v\n", cfg.Verbose)
	return os.WriteFile(path, []byte(b.String()), 0600)
}

// CreateDefaultConfig creates a default config.ini and returns its path.
func CreateDefaultConfig() string {
	hostname, _ := os.Hostname()
	cfg := &Config{
		PlayerName: hostname,
		RoomID:     "default",
	}
	SaveConfig(cfg)
	return PortableConfigPath()
}

// loadINI parses a key=value config file into cfg. Returns true if file exists.
func loadINI(path string, cfg *Config) bool {
	m, ok := iniconfig.ParseFile(path)
	if !ok {
		return false
	}
	if v := m["server"]; v != "" {
		cfg.ServerAddr = v
	}
	if v := m["name"]; v != "" {
		cfg.PlayerName = v
	}
	if v := m["room"]; v != "" {
		cfg.RoomID = v
	}
	if v := m["password"]; v != "" {
		cfg.RoomPassword = v
	}
	if v := m["lang"]; v != "" {
		cfg.Lang = v
	}
	if v := m["mtu"]; v != "" {
		if mtu, err := strconv.Atoi(v); err == nil && mtu >= MinMTU && mtu <= MaxMTU {
			cfg.MTU = mtu
		}
	}
	if v := m["log-file"]; v != "" {
		cfg.LogFile = v
	}
	if v := m["verbose"]; v != "" {
		cfg.Verbose = v == "true" || v == "1"
	}
	cfg.ServerAddr = iniconfig.CombinePort(cfg.ServerAddr, m["port"])
	return true
}

// loadJSON parses the legacy AppData JSON config into cfg.
func loadJSON(path string, cfg *Config) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	type raw struct {
		ServerAddr   string `json:"server_addr"`
		PlayerName   string `json:"player_name"`
		RoomID       string `json:"room_id"`
		RoomPassword string `json:"room_password,omitempty"`
		AutoConnect  *bool  `json:"auto_connect,omitempty"`
		Lang         string `json:"lang,omitempty"`
		MTU          int    `json:"mtu,omitempty"`
		LogFile      string `json:"log_file,omitempty"`
		Verbose      *bool  `json:"verbose,omitempty"`
	}
	var r raw
	if json.Unmarshal(data, &r) == nil {
		if r.ServerAddr != "" {
			cfg.ServerAddr = r.ServerAddr
		}
		if r.PlayerName != "" {
			cfg.PlayerName = r.PlayerName
		}
		if r.RoomID != "" {
			cfg.RoomID = r.RoomID
		}
		cfg.RoomPassword = r.RoomPassword
		if r.Lang != "" {
			cfg.Lang = r.Lang
		}
		if r.MTU >= MinMTU && r.MTU <= MaxMTU {
			cfg.MTU = r.MTU
		}
		if r.LogFile != "" {
			cfg.LogFile = r.LogFile
		}
		if r.Verbose != nil {
			cfg.Verbose = *r.Verbose
		}
	}
}
