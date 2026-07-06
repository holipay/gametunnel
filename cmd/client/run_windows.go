//go:build windows

package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"syscall"

	"github.com/lxn/walk"

	"github.com/holipay/gametunnel/internal/auth"
	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/client/ui"
	"github.com/holipay/gametunnel/internal/logfile"
)

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

	if _, warnings := auth.CheckPasswordStrength(cfg.RoomPassword); len(warnings) > 0 {
		for _, w := range warnings {
			log.Printf("[auth] %s", w)
		}
	}

	app := client.NewApp(cfg)
	app.SetTUNFactory(tunFactory)

	// Create owner window
	owner, err := walk.NewMainWindow()
	if err != nil {
		log.Fatalf("create owner window: %v", err)
	}
	owner.SetTitle("GameTunnel")
	owner.SetBounds(walk.Rectangle{X: -32000, Y: -32000, Width: 1, Height: 1})

	// Show settings dialog on first run
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

	// Create tray
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
	log.Printf("GameTunnel Client %s (commit: %s, built: %s)", Version, Commit, BuildTime)

	hideConsole()

	// Safety net: if the owner window gets destroyed, recreate it
	// so the message loop stays alive.
	go func() {
		kernel32 := syscall.NewLazyDLL("kernel32.dll")
		sleep := kernel32.NewProc("Sleep")
		for {
			sleep.Call(2000)
			if owner.IsDisposed() {
				log.Printf("[ui] owner window lost, recreating...")
				newOwner, err := walk.NewMainWindow()
				if err != nil {
					log.Printf("[ui] failed to recreate owner: %v", err)
					continue
				}
				newOwner.SetTitle("GameTunnel")
				newOwner.SetBounds(walk.Rectangle{X: -32000, Y: -32000, Width: 1, Height: 1})
				owner = newOwner
			}
		}
	}()

	// Run walk message loop
	owner.Run()

	// Cleanup
	if pprofLn != nil {
		pprofLn.Close()
	}
	app.Disconnect()
	fmt.Println("Disconnected.")
}
