//go:build !windows

package main

import (
	"os/exec"
	"runtime"

	"github.com/holipay/gametunnel/internal/client"
)

// openLogFile opens the log file with the default text editor.
func openLogFile() {
	logPath := appDataPath() + "/GameTunnel/gametunnel.log"
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", logPath)
	default:
		cmd = exec.Command("xdg-open", logPath)
	}
	cmd.Start()
}

// openConfigFile opens config.ini in the default text editor.
func openConfigFile() {
	path := client.PortableConfigPath()
	cmd := exec.Command("xdg-open", path)
	cmd.Start()
}

// setupFirewallPlatform is a no-op on non-Windows platforms.
func setupFirewallPlatform() (func(), error) {
	return func() {}, nil
}
