package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/holipay/gametunnel/internal/client"
)

// run is the cross-platform entry point. It sets up the HTTP server,
// system tray, and starts the application.
func run(cfg *client.Config, tunFactory func(client.TunConfig) (client.TunDevice, error)) {
	// Setup logging
	logFile := setupLog()
	defer logFile.Close()

	// Setup firewall (Windows-only, no-op on other platforms)
	cleanup, _ := setupFirewallPlatform()
	defer cleanup()

	// Create app
	app := NewApp(cfg)
	app.SetTUNFactory(tunFactory)

	// Create HTTP server on localhost only
	httpSrv := NewHTTPServer(app, "127.0.0.1:4702")

	// Start HTTP server in background
	go func() {
		if err := httpSrv.Start(); err != nil {
			log.Printf("[http] 服务停止: %v", err)
		}
	}()

	// Give HTTP server a moment to start
	time.Sleep(100 * time.Millisecond)

	// Auto-connect if server is configured
	if cfg.ServerAddr != "" {
		log.Printf("[app] 自动连接到 %s", cfg.ServerAddr)
		app.Connect(cfg)
	}

	// Open dashboard in browser
	go func() {
		time.Sleep(500 * time.Millisecond)
		openBrowser(fmt.Sprintf("http://127.0.0.1%s", httpSrv.addr))
	}()

	fmt.Println("  GameTunnel 已启动")
	fmt.Printf("  控制面板: http://127.0.0.1%s\n", httpSrv.addr)
	fmt.Println("  系统托盘图标已就绪")

	// Run tray (blocks until quit)
	RunTray(app, httpSrv)
}

// setupLog configures logging to file + stderr.
func setupLog() *os.File {
	logDir := filepath.Join(appDataPath(), "GameTunnel")
	os.MkdirAll(logDir, 0755)
	logPath := filepath.Join(logDir, "gametunnel.log")

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		log.SetOutput(os.Stderr)
		return os.Stderr
	}
	log.SetOutput(io.MultiWriter(f, os.Stderr))
	log.Printf("=== GameTunnel 启动 ===")
	return f
}

// appDataPath returns the AppData directory path.
func appDataPath() string {
	// Windows
	if appData := os.Getenv("APPDATA"); appData != "" {
		return appData
	}
	if userProfile := os.Getenv("USERPROFILE"); userProfile != "" {
		return filepath.Join(userProfile, "AppData", "Roaming")
	}
	// Unix fallback
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	return "."
}
