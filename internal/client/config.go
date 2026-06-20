package client

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/holipay/gametunnel/internal/i18n"
)

// Config holds client configuration.
type Config struct {
	ServerAddr   string
	PlayerName   string
	RoomID       string
	RoomPassword string
	Lang         string // "zh" or "en", default "zh"
	MTU          int    // tunnel MTU, default 1400
}

// exeDir returns the directory containing the executable.
func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

// PortableConfigPath returns the path to config.ini next to the exe.
func PortableConfigPath() string {
	return filepath.Join(exeDir(), "config.ini")
}

// appDataPath returns the AppData directory path.
func appDataPath() string {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		appData = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Roaming")
	}
	return appData
}

// LegacyConfigPath returns the path to the legacy JSON config file.
func LegacyConfigPath() string {
	return filepath.Join(appDataPath(), "GameTunnel", "config.json")
}

// LoadConfig loads the config from disk.
// Priority: config.ini next to exe > AppData/config.json > defaults.
func LoadConfig() *Config {
	hostname, _ := os.Hostname()
	cfg := &Config{
		PlayerName: hostname,
		RoomID:     "default",
		MTU:        1400,
	}

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
	}
	if portNum < 1 || portNum > 65535 {
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
	fmt.Fprintf(&b, "server=%s\n", cfg.ServerAddr)
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
	return os.WriteFile(path, []byte(b.String()), 0644)
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
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
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
		case "server":
			cfg.ServerAddr = value
		case "name":
			if value != "" {
				cfg.PlayerName = value
			}
		case "room":
			if value != "" {
				cfg.RoomID = value
			}
		case "password":
			cfg.RoomPassword = value
		case "lang":
			if value != "" {
				cfg.Lang = value
			}
		case "mtu":
			var v int
			if _, err := fmt.Sscanf(value, "%d", &v); err == nil && v >= 576 && v <= 9000 {
				cfg.MTU = v
			}
		}
	}
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
		if r.MTU >= 576 && r.MTU <= 9000 {
			cfg.MTU = r.MTU
		}
	}
}
