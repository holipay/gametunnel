package client

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config holds client configuration.
type Config struct {
	ServerAddr   string
	PlayerName   string
	RoomID       string
	RoomPassword string
	Lang         string // "zh" or "en", default "zh"
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
	}

	// Try portable config.ini first
	if loadINI(PortableConfigPath(), cfg) {
		return cfg
	}

	// Fall back to legacy JSON config
	loadJSON(LegacyConfigPath(), cfg)
	return cfg
}

// SaveConfig writes the config to config.ini next to the exe.
func SaveConfig(cfg *Config) error {
	path := PortableConfigPath()
	var b strings.Builder
	fmt.Fprintln(&b, "# GameTunnel Configuration")
	fmt.Fprintln(&b, "# Server address (required, e.g. 1.2.3.4:4700)")
	fmt.Fprintf(&b, "server=%s\n", cfg.ServerAddr)
	fmt.Fprintln(&b, "# Player name (default: computer name)")
	fmt.Fprintf(&b, "name=%s\n", cfg.PlayerName)
	fmt.Fprintln(&b, "# Room ID (default: default)")
	fmt.Fprintf(&b, "room=%s\n", cfg.RoomID)
	fmt.Fprintln(&b, "# Password (leave empty if none)")
	fmt.Fprintf(&b, "password=%s\n", cfg.RoomPassword)
	fmt.Fprintln(&b, "# Language (zh or en)")
	fmt.Fprintf(&b, "lang=%s\n", cfg.Lang)
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
	}
}
