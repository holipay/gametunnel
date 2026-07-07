//go:build windows

package tun

import (
	"fmt"
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000 // CREATE_NO_WINDOW

// RunCmd executes a system command and returns an error with combined output
// if the command fails.
//
// On Windows, CREATE_NO_WINDOW prevents the subprocess from allocating a
// console, which avoids the black console window flash that HideWindow alone
// cannot suppress (HideWindow only hides the window after it's created).
func RunCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %s", name, args, string(out))
	}
	return nil
}
