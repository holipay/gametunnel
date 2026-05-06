package client

import (
	"io"
	"log"
	"os"
	"path/filepath"
)

// SetupLog configures logging to both file and stderr (tee).
// Returns the log file handle (caller should defer Close).
func SetupLog() *os.File {
	logDir := filepath.Join(appDataPath(), "GameTunnel")
	os.MkdirAll(logDir, 0755)
	logPath := filepath.Join(logDir, "gametunnel.log")

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		log.SetOutput(os.Stderr)
		return os.Stderr
	}
	log.SetOutput(io.MultiWriter(f, os.Stderr))
	log.Printf("=== GameTunnel 启动 ===")
	return f
}
