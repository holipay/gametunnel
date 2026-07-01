//go:build windows

// Package crashlog provides a cross-platform panic recovery logger.
package crashlog

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/holipay/gametunnel/internal/paths"
)

// WriteCrashLog handles a panic by writing it to crash.log.
// On Windows, this silently returns after writing the log.
func WriteCrashLog(label string) {
	r := recover()
	if r == nil {
		return
	}

	logDir := paths.GameTunnelDir()
	os.MkdirAll(logDir, 0755)
	f, err := os.OpenFile(filepath.Join(logDir, "crash.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return
	}
	defer f.Close()

	fmt.Fprintf(f, "=== Crash %s ===\n", label)
	fmt.Fprintf(f, "Panic: %v\n\n", r)
	fmt.Fprintf(f, "Stack:\n%s\n", debug.Stack())
}
