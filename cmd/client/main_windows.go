//go:build windows

package main

import (
	"fmt"
	"log"
	"os"

	"golang.org/x/sys/windows"

	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/crashlog"
	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/singleinstance"
	"github.com/holipay/gametunnel/internal/tun"
)

func main() {
	defer crashlog.WriteCrashLog("GameTunnel Client")

	// Request admin rights if not elevated (needed for TUN device)
	requestAdmin()

	windows.SetConsoleOutputCP(65001)

	// Prevent multiple instances
	if _, err := singleinstance.Acquire("GameTunnel-Client"); err != nil {
		log.Printf("single instance check: %v", err)
		fmt.Println("GameTunnel is already running.")
		os.Exit(0)
	}

	// Load config
	cfg := client.LoadConfig()

	// Set language from config
	if cfg.Lang != "" {
		i18n.Set(i18n.ParseLang(cfg.Lang))
	}

	// Parse server public IP for route exclusion
	serverPublicIP := parseHostIP(cfg.ServerAddr)

	// Setup TUN factory for Windows
	tunFactory := func(tunCfg client.TunConfig) (client.TunDevice, error) {
		return tun.New(tun.Config{
			VirtualIP:      tunCfg.VirtualIP,
			SubnetMask:     tunCfg.SubnetMask,
			ServerIP:       tunCfg.ServerIP,
			ServerPublicIP: serverPublicIP,
			MTU:            tunCfg.MTU,
		})
	}

	run(cfg, tunFactory)
}

func requestAdmin() {
	token := windows.GetCurrentProcessToken()
	if token.IsElevated() {
		return
	}

	exe, err := os.Executable()
	if err != nil {
		return
	}

	verb, _ := windows.UTF16PtrFromString("runas")
	exePath, _ := windows.UTF16PtrFromString(exe)

	if err := windows.ShellExecute(0, verb, exePath, nil, nil, windows.SW_SHOWNORMAL); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to elevate: %v\n", err)
		os.Exit(1)
	}

	os.Exit(0)
}
