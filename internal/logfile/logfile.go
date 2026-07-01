// Package logfile provides shared log file setup with rotation.
package logfile

import (
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/holipay/gametunnel/internal/paths"
)

// MaxLogSize is the maximum log file size before rotation (1 MB).
const MaxLogSize = 1 * 1024 * 1024

// Setup creates the log directory, rotates the log file if it exceeds
// MaxLogSize, and sets log output to both the file and stderr.
// Returns the log file handle (caller must close).
func Setup() *os.File {
	logDir := paths.GameTunnelDir()
	if err := os.MkdirAll(logDir, 0755); err != nil {
		log.SetOutput(os.Stderr)
		log.Printf("create log dir: %v", err)
		return os.Stderr
	}
	logPath := filepath.Join(logDir, "gametunnel.log")
	logBackup := filepath.Join(logDir, "gametunnel.log.1")

	if info, err := os.Stat(logPath); err == nil && info.Size() > MaxLogSize {
		os.Remove(logBackup)
		if err := os.Rename(logPath, logBackup); err != nil {
			log.Printf("rotate log: %v", err)
		}
	}

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		log.SetOutput(os.Stderr)
		return os.Stderr
	}
	log.SetOutput(io.MultiWriter(f, os.Stderr))
	return f
}
