//go:build windows

package main

import (
	"fmt"
	"net"
	"os"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/tun"
)

var (
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	user32               = syscall.NewLazyDLL("user32.dll")
	procShowWindow       = user32.NewProc("ShowWindow")
)

func main() {
	windows.SetConsoleOutputCP(65001)

	// Hide the console window (we only use the tray icon)
	hideConsole()

	// Request admin rights if not elevated
	requestAdmin()

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

// hideConsole hides the console window.
func hideConsole() {
	hwnd, _, _ := procGetConsoleWindow.Call()
	if hwnd != 0 {
		procShowWindow.Call(hwnd, 0) // SW_HIDE = 0
	}
}

// requestAdmin checks if the process is running with admin rights.
// If not, re-launches with "runas" verb (UAC prompt).
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
		fmt.Fprintf(os.Stderr, "  Failed to elevate: %v\n", err)
		os.Exit(1)
	}

	os.Exit(0)
}

// parseHostIP extracts the IP from a "host:port" address string.
// Returns nil if parsing fails.
func parseHostIP(addr string) net.IP {
	host, err := net.SplitHostPort(addr)
	if err != nil {
		// Try as bare IP
		return net.ParseIP(addr)
	}
	return net.ParseIP(host)
}
