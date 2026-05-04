package gui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const configDir = "GameTunnel"
const configFile = "config.json"

// Config holds user settings persisted to disk.
type Config struct {
	ServerAddr string `json:"server_addr"` // e.g. "1.2.3.4:4700"
	PlayerName string `json:"player_name"` // display name
	RoomID     string `json:"room_id"`     // game room identifier
	AutoConnect bool  `json:"auto_connect"` // connect on startup
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() *Config {
	hostname, _ := os.Hostname()
	return &Config{
		ServerAddr: "127.0.0.1:4700",
		PlayerName: hostname,
		RoomID:     "default",
		AutoConnect: false,
	}
}

// ConfigPath returns %APPDATA%\GameTunnel\config.json
func ConfigPath() string {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		appData = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Roaming")
	}
	return filepath.Join(appData, configDir, configFile)
}

// LoadConfig reads config from disk, or returns defaults.
func LoadConfig() *Config {
	cfg := DefaultConfig()
	data, err := os.ReadFile(ConfigPath())
	if err != nil {
		return cfg
	}
	json.Unmarshal(data, cfg)
	return cfg
}

// SaveConfig writes config to disk.
func SaveConfig(cfg *Config) error {
	path := ConfigPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
