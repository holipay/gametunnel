package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/holipay/gametunnel/internal/auth"
	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/logfile"
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
	logFile := logfile.Setup()
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

	// Password strength warning
	if _, warnings := auth.CheckPasswordStrength(cfg.RoomPassword); len(warnings) > 0 {
		for _, w := range warnings {
			fmt.Printf("[auth] %s\n", w)
		}
	}

	if cfg.ServerAddr == "" {
		fmt.Println("No server configured. Edit config.ini and set server=address:port")
		return
	}

	app := client.NewApp(cfg)
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

// parseHostIP extracts the IP from an address string (e.g. "1.2.3.4:4700" or "[::1]:4700").
func parseHostIP(addr string) net.IP {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return net.ParseIP(addr)
	}
	return net.ParseIP(host)
}
