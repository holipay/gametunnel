//go:build windows

package main

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"

	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/tun"
)

// openLogFile opens the log file with the default text editor (Windows).
func openLogFile() {
	logPath := filepath.Join(appDataPath(), "GameTunnel", "gametunnel.log")
	if _, err := os.Stat(logPath); err != nil {
		os.MkdirAll(filepath.Dir(logPath), 0755)
		os.WriteFile(logPath, []byte(""), 0600)
	}
	windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(logPath), nil, nil, windows.SW_SHOWNORMAL)
}

// openConfigFile opens config.ini in the default text editor (Windows).
// Creates the file with defaults if it doesn't exist.
func openConfigFile() {
	path := client.PortableConfigPath()
	if _, err := os.Stat(path); err != nil {
		client.CreateDefaultConfig()
	}
	windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(path), nil, nil, windows.SW_SHOWNORMAL)
}

// setupFirewallPlatform adds Windows Firewall rules. Returns cleanup func.
func setupFirewallPlatform() (func(), error) {
	cleanup, err := tun.SetupFirewall()
	if err != nil {
		return func() {}, err
	}
	return cleanup, nil
}
