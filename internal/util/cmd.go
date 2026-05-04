// Package util provides shared helpers used across packages.
package util

import (
	"fmt"
	"os/exec"
)

// RunCmd executes a system command and returns an error with combined output
// if the command fails.
func RunCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %s", name, args, string(out))
	}
	return nil
}
