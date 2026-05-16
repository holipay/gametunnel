package main

import (
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/i18n"
)

// run is the cross-platform entry point. It sets up logging, firewall,
// and starts the system tray.
func run(cfg *client.Config, tunFactory func(client.TunConfig) (client.TunDevice, error)) {
	logFile := setupLog()
	defer logFile.Close()

	cleanup, _ := setupFirewallPlatform()
	defer cleanup()

	app := NewApp(cfg)
	app.SetTUNFactory(tunFactory)

	// Auto-connect if server is configured
	if cfg.ServerAddr != "" {
		log.Printf("%s", i18n.Format(i18n.T().AppAutoConnect, cfg.ServerAddr))
		app.Connect(cfg)
	}

	// Run tray (blocks until quit)
	RunTray(app)
}

// maxLogSize is the maximum log file size before rotation (1 MB).
const maxLogSize = 1 * 1024 * 1024

func setupLog() *os.File {
	logDir := filepath.Join(appDataPath(), "GameTunnel")
	os.MkdirAll(logDir, 0755)
	logPath := filepath.Join(logDir, "gametunnel.log")

	// Rotate: if log exceeds maxLogSize, truncate on open.
	if info, err := os.Stat(logPath); err == nil && info.Size() > maxLogSize {
		os.Remove(logPath)
	}

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		log.SetOutput(os.Stderr)
		return os.Stderr
	}
	log.SetOutput(io.MultiWriter(f, os.Stderr))
	log.Printf("%s", i18n.T().RunStartup)
	return f
}

func appDataPath() string {
	if appData := os.Getenv("APPDATA"); appData != "" {
		return appData
	}
	if userProfile := os.Getenv("USERPROFILE"); userProfile != "" {
		return filepath.Join(userProfile, "AppData", "Roaming")
	}
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	return "."
}
