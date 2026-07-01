//go:build !windows

package crashlog

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/holipay/gametunnel/internal/paths"
)

// WriteCrashLog handles a panic by writing it to crash.log and stderr, then exits.
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
		fmt.Fprintf(os.Stderr, "Panic: %v\n%s\n", r, debug.Stack())
		os.Exit(1)
	}
	defer f.Close()

	fmt.Fprintf(f, "=== Crash %s ===\n", label)
	fmt.Fprintf(f, "Panic: %v\n\n", r)
	fmt.Fprintf(f, "Stack:\n%s\n", debug.Stack())
	fmt.Fprintf(os.Stderr, "Panic: %v\nSee crash.log for details.\n", r)
	os.Exit(1)
}
