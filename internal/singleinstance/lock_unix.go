//go:build !windows

package singleinstance

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// Lock holds a file-based exclusive lock (Unix).
type Lock struct {
	file *os.File
	path string
}

// Close releases the flock and removes the lock file.
func (l *Lock) Close() error {
	var err error
	if l.file != nil {
		// Release the flock explicitly (released automatically on close, but be polite)
		syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
		err = l.file.Close()
	}
	os.Remove(l.path)
	return err
}

// Acquire attempts to acquire a single-instance lock using flock(2).
// Returns an error if another instance is already running.
func Acquire(name string) (*Lock, error) {
	lockPath := filepath.Join(os.TempDir(), name+".lock")

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		holdingPID := readPIDFile(lockPath)
		f.Close()
		if holdingPID > 0 && !processAlive(holdingPID) {
			os.Remove(lockPath)
			return Acquire(name) // retry once after stale cleanup
		}
		if holdingPID > 0 {
			return nil, fmt.Errorf("another instance is already running (PID %d)", holdingPID)
		}
		return nil, fmt.Errorf("another instance is already running")
	}

	writePIDFile(f)
	return &Lock{file: f, path: lockPath}, nil
}

func writePIDFile(f *os.File) {
	f.Truncate(0)
	f.Seek(0, 0)
	f.Write([]byte(strconv.Itoa(os.Getpid())))
	f.Sync()
}

func readPIDFile(path string) int {
	data, _ := os.ReadFile(path)
	pid, _ := strconv.Atoi(string(data))
	return pid
}

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
