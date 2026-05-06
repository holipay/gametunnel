package client

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config holds client configuration.
type Config struct {
	ServerAddr   string `json:"server_addr"`
	PlayerName   string `json:"player_name"`
	RoomID       string `json:"room_id"`
	RoomPassword string `json:"room_password,omitempty"`
}

// appDataPath returns the AppData directory path.
func appDataPath() string {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		appData = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Roaming")
	}
	return appData
}

// ConfigPath returns the path to the config file.
func ConfigPath() string {
	return filepath.Join(appDataPath(), "GameTunnel", "config.json")
}

// LoadConfig loads the config from disk, with defaults.
func LoadConfig() *Config {
	hostname, _ := os.Hostname()
	cfg := &Config{
		PlayerName: hostname,
		RoomID:     "default",
	}
	data, err := os.ReadFile(ConfigPath())
	if err != nil {
		return cfg
	}
	// Backward-compatible: ignore unknown fields (e.g. auto_connect)
	type raw struct {
		ServerAddr   string `json:"server_addr"`
		PlayerName   string `json:"player_name"`
		RoomID       string `json:"room_id"`
		RoomPassword string `json:"room_password,omitempty"`
		AutoConnect  *bool  `json:"auto_connect,omitempty"` // ignored
	}
	var r raw
	if err := json.Unmarshal(data, &r); err == nil {
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
	}
	return cfg
}

// SaveConfig writes the config to disk with 0600 permissions (protects password).
func SaveConfig(cfg *Config) error {
	path := ConfigPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}
