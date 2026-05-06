package client

import (
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
	log.SetOutput(newTeeWriter(f, os.Stderr))
	log.Printf("=== GameTunnel 启动 ===")
	return f
}

// teeWriter writes to two writers (for log → file + stderr).
type teeWriter struct {
	a, b *os.File
}

func newTeeWriter(a, b *os.File) *teeWriter {
	return &teeWriter{a: a, b: b}
}

func (t *teeWriter) Write(p []byte) (n int, err error) {
	n1, err1 := t.a.Write(p)
	n2, err2 := t.b.Write(p)
	if err1 != nil {
		return n1, err1
	}
	if err2 != nil {
		return n2, err2
	}
	if n1 < n2 {
		return n1, nil
	}
	return n2, nil
}
