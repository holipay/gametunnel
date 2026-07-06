//go:build windows

package main

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/crashlog"
	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/singleinstance"
	"github.com/holipay/gametunnel/internal/tun"
)

func main() {
	defer crashlog.WriteCrashLog("GameTunnel Client")

	// Lock OS thread for Win32 GUI (walk requirement)
	runtime.LockOSThread()

	// Request admin rights if not elevated (needed for TUN device)
	requestAdmin()

	// Hide console window (GUI takes over)
	hideConsole()

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

	// Launch GUI
	runWindows(cfg, tunFactory)
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

func hideConsole() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	user32 := syscall.NewLazyDLL("user32.dll")

	procGetConsoleWindow := kernel32.NewProc("GetConsoleWindow")
	procShowWindow := user32.NewProc("ShowWindow")

	hwnd, _, _ := procGetConsoleWindow.Call()
	if hwnd != 0 {
		procShowWindow.Call(hwnd, 0) // SW_HIDE
	}
}
