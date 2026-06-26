package main

import (
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/i18n"
)

// Build info, set at build time via -ldflags.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

// run is the cross-platform entry point. It sets up logging, firewall,
// and starts the system tray.
func run(cfg *client.Config, tunFactory func(client.TunConfig) (client.TunDevice, error)) {
	logFile := setupLog()
	defer func() {
		if logFile != os.Stderr {
			logFile.Close()
		}
	}()

	cleanup, err := setupFirewallPlatform()
	if err != nil {
		log.Printf("firewall setup failed: %v (non-fatal)", err)
	}
	defer cleanup()

	app := NewApp(cfg)
	app.SetTUNFactory(tunFactory)

	// Start embedded web UI for settings
	webui := client.NewWebUI(app)
	webui.Start("127.0.0.1:4702")
	defer webui.Stop()

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

// maxLogBackups is the number of old log files to keep.
const maxLogBackups = 1

func setupLog() *os.File {
	logDir := filepath.Join(appDataPath(), "GameTunnel")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		log.SetOutput(os.Stderr)
		log.Printf("create log dir: %v", err)
		return os.Stderr
	}
	logPath := filepath.Join(logDir, "gametunnel.log")
	logBackup := filepath.Join(logDir, "gametunnel.log.1")

	// Rotate: if log exceeds maxLogSize, rename to backup
	if info, err := os.Stat(logPath); err == nil && info.Size() > maxLogSize {
		os.Remove(logBackup)
		if err := os.Rename(logPath, logBackup); err != nil {
			log.Printf("rotate log: %v", err)
		}
	}

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		log.SetOutput(os.Stderr)
		return os.Stderr
	}
	log.SetOutput(io.MultiWriter(f, os.Stderr))
	log.Printf("GameTunnel Client %s (commit: %s, built: %s)", Version, Commit, BuildTime)
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
