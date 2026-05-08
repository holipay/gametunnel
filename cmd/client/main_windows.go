//go:build windows

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"

	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/tun"
)

func main() {
	// Set console to UTF-8
	windows.SetConsoleOutputCP(65001)

	// Request admin rights if not elevated
	requestAdmin()

	// Load config
	cfg := client.LoadConfig()

	// Setup TUN factory for Windows
	tunFactory := func(tunCfg client.TunConfig) (client.TunDevice, error) {
		return tun.New(tun.Config{
			VirtualIP:  tunCfg.VirtualIP,
			SubnetMask: tunCfg.SubnetMask,
			ServerIP:   tunCfg.ServerIP,
			MTU:        tunCfg.MTU,
		})
	}

	// Run the application
	run(cfg, tunFactory)
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
		fmt.Fprintf(os.Stderr, "  无法提升权限: %v\n", err)
		os.Exit(1)
	}

	// Exit the non-elevated process
	os.Exit(0)
}
