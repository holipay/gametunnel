//go:build windows

package singleinstance

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Lock holds a Windows named mutex.
type Lock struct {
	handle windows.Handle
}

// Close releases the named mutex.
func (l *Lock) Close() error {
	if l.handle != 0 {
		return windows.ReleaseMutex(l.handle)
	}
	return nil
}

// Acquire attempts to acquire a named mutex.
// Returns an error if another instance is already running.
func Acquire(name string) (*Lock, error) {
	nameUTF16, err := windows.UTF16PtrFromString("Global\\" + name)
	if err != nil {
		return nil, fmt.Errorf("invalid mutex name: %w", err)
	}

	// CreateMutex with bInitialOwner=true: we own it immediately if it didn't exist.
	// If it already exists, GetLastError returns ERROR_ALREADY_EXISTS.
	handle, err := windows.CreateMutex(nil, true, nameUTF16)
	if err != nil {
		return nil, fmt.Errorf("create mutex: %w", err)
	}

	// Check if the mutex already existed before we created it.
	lastErr := windows.GetLastError()
	if lastErr == windows.ERROR_ALREADY_EXISTS {
		windows.CloseHandle(handle)
		return nil, fmt.Errorf("another instance is already running")
	}

	return &Lock{handle: handle}, nil
}

// Ensure we use the unsafe import (needed for UTF16PtrFromString on some Go versions).
var _ = unsafe.Pointer(nil)
