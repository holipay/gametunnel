//go:build windows

package tun

import (
	"fmt"
	"os/exec"
	"syscall"
)

// RunCmd executes a system command and returns an error with combined output
// if the command fails.
//
// On Windows, the subprocess window is hidden via SysProcAttr.HideWindow
// to prevent PowerShell/cmd windows from flashing on screen.
func RunCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %s", name, args, string(out))
	}
	return nil
}
