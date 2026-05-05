package client

import (
	"encoding/json"
	"log"
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

// configPath returns the path to the config file.
func configPath() string {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		appData = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Roaming")
	}
	return filepath.Join(appData, "GameTunnel", "config.json")
}

// LoadConfig loads the config from disk, with defaults.
func LoadConfig() *Config {
	hostname, _ := os.Hostname()
	cfg := &Config{
		PlayerName: hostname,
		RoomID:     "default",
	}
	data, err := os.ReadFile(configPath())
	if err != nil {
		return cfg
	}
	type raw struct {
		ServerAddr   string `json:"server_addr"`
		PlayerName   string `json:"player_name"`
		RoomID       string `json:"room_id"`
		RoomPassword string `json:"room_password,omitempty"`
		AutoConnect  *bool  `json:"auto_connect,omitempty"` // ignored, backward compat
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

// SaveConfig writes the config to disk.
func SaveConfig(cfg *Config) error {
	path := configPath()
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

// SetupLog configures logging to both file and stderr.
func SetupLog() *os.File {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		appData = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Roaming")
	}
	logDir := filepath.Join(appData, "GameTunnel")
	os.MkdirAll(logDir, 0755)
	logPath := filepath.Join(logDir, "gametunnel.log")

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return os.Stderr
	}
	// Write to both file and stderr (tee)
	log.SetOutput(newTeeWriter(f, os.Stderr))
	log.Printf("=== GameTunnel 启动 ===")
	return f
}

// teeWriter writes to two writers.
type teeWriter struct {
	a, b *os.File
}

func newTeeWriter(a, b *os.File) *teeWriter {
	return &teeWriter{a: a, b: b}
}

func (t *teeWriter) Write(p []byte) (n int, err error) {
	n1, err1 := t.a.Write(p)
	n2, err2 := t.b.Write(p)
	if err1 != nil {
		return n1, err1
	}
	if err2 != nil {
		return n2, err2
	}
	if n1 < n2 {
		return n1, nil
	}
	return n2, nil
}
