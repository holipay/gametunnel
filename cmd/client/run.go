package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/i18n"
)

// Build info, set at build time via -ldflags.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

// run is the cross-platform entry point. It loads config, connects to the
// server, and blocks until SIGINT/SIGTERM.
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

	fmt.Printf("GameTunnel Client %s (commit: %s, built: %s)\n", Version, Commit, BuildTime)

	if cfg.ServerAddr == "" {
		fmt.Println("No server configured. Edit config.ini and set server=address:port")
		return
	}

	app := NewApp(cfg)
	app.SetTUNFactory(tunFactory)

	log.Printf("%s", i18n.Format(i18n.T().AppAutoConnect, cfg.ServerAddr))
	app.Connect(cfg)

	// Wait for SIGINT/SIGTERM
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh

	fmt.Printf("\nReceived %v, disconnecting...\n", sig)
	app.Disconnect()
	fmt.Println("Disconnected.")
}

// maxLogSize is the maximum log file size before rotation (1 MB).
const maxLogSize = 1 * 1024 * 1024

func setupLog() *os.File {
	logDir := filepath.Join(appDataPath(), "GameTunnel")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		log.SetOutput(os.Stderr)
		log.Printf("create log dir: %v", err)
		return os.Stderr
	}
	logPath := filepath.Join(logDir, "gametunnel.log")
	logBackup := filepath.Join(logDir, "gametunnel.log.1")

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
	return f
}

// parseHostIP extracts the IP from an address string (e.g. "1.2.3.4:4700" or "[::1]:4700").
func parseHostIP(addr string) net.IP {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return net.ParseIP(addr)
	}
	return net.ParseIP(host)
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
