//go:build windows

package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"syscall"
	"unsafe"

	"github.com/lxn/walk"
	"github.com/lxn/win"

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

	// Custom message loop: pump messages on the owner's thread.
	// This loop does NOT depend on owner.hWnd — it processes all
	// thread messages. The loop only exits when tray "Exit" calls
	// walk.App().Exit(0) which posts WM_QUIT.
	msg := (*win.MSG)(unsafe.Pointer(win.GlobalAlloc(0, unsafe.Sizeof(win.MSG{}))))
	defer win.GlobalFree(win.HGLOBAL(unsafe.Pointer(msg)))

	user32 := syscall.NewLazyDLL("user32.dll")
	getMessage := user32.NewProc("GetMessageW")
	translate := user32.NewProc("TranslateMessage")
	dispatch := user32.NewProc("DispatchMessageW")

	for {
		ret, _, _ := getMessage.Call(
			uintptr(unsafe.Pointer(msg)),
			0, 0, 0,
		)
		if ret == 0 { // WM_QUIT
			break
		}
		if ret == ^uintptr(0) { // -1 = error
			break
		}
		translate.Call(uintptr(unsafe.Pointer(msg)))
		dispatch.Call(uintptr(unsafe.Pointer(msg)))
	}

	// Cleanup
	if pprofLn != nil {
		pprofLn.Close()
	}
	app.Disconnect()
	fmt.Println("Disconnected.")
}
