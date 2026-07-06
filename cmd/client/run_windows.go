//go:build windows

package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"

	"github.com/lxn/walk"

	"github.com/holipay/gametunnel/internal/auth"
	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/client/ui"
	"github.com/holipay/gametunnel/internal/logfile"
)

// runWindows is the Windows GUI entry point. It starts the system tray,
// main window, and manages the app lifecycle.
func runWindows(cfg *client.Config, tunFactory func(client.TunConfig) (client.TunDevice, error)) {
	logFile := logfile.Setup(cfg.LogFile)
	defer func() {
		if logFile != nil {
			logFile.Close()
		}
	}()

	cleanup, err := setupFirewallPlatform()
	if err != nil {
		log.Printf("firewall setup failed: %v (non-fatal)", err)
	}
	defer cleanup()

	// Password strength warning
	if _, warnings := auth.CheckPasswordStrength(cfg.RoomPassword); len(warnings) > 0 {
		for _, w := range warnings {
			log.Printf("[auth] %s", w)
		}
	}

	// Create the app
	app := client.NewApp(cfg)
	app.SetTUNFactory(tunFactory)

	// Create a hidden owner window (needed for tray and dialog)
	owner, err := walk.NewMainWindow()
	if err != nil {
		log.Fatalf("create owner window: %v", err)
	}
	owner.SetTitle("GameTunnel")
	owner.SetSize(walk.Size{Width: 1, Height: 1})
	owner.SetVisible(false)

	// Show settings dialog on first run (no server configured)
	if cfg.ServerAddr == "" {
		newCfg := ui.ShowSettingsDialog(owner, cfg)
		if newCfg == nil {
			owner.Dispose()
			return
		}
		cfg = newCfg
		app.Cfg = cfg
	}

	// pprof
	var pprofLn net.Listener
	if cfg.PprofAddr != "" {
		pprofLn, err = net.Listen("tcp", cfg.PprofAddr)
		if err != nil {
			log.Fatalf("pprof listen: %v", err)
		}
		go func() {
			log.Printf("pprof listening on %s", pprofLn.Addr())
			if http.Serve(pprofLn, nil) != nil {
				log.Printf("pprof server stopped")
			}
		}()
	}

	// Create tray (uses owner as parent)
	tray, err := ui.NewTray(app, owner)
	if err != nil {
		log.Fatalf("create tray: %v", err)
	}
	defer tray.Dispose()

	// Create status window
	sw, err := ui.NewStatusWindow(app, tray, owner)
	if err != nil {
		log.Fatalf("create status window: %v", err)
	}
	defer sw.Dispose()
	tray.SetMainWindow(sw.Window())

	// Connect
	app.Connect(cfg)
	log.Printf("GameTunnel Client %s started (commit: %s)", Version, Commit)

	// Run the walk message loop (blocks until exit)
	owner.Run()

	// Cleanup
	if pprofLn != nil {
		pprofLn.Close()
	}
	app.Disconnect()
	fmt.Println("Disconnected.")
}
