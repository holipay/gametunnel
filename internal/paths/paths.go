// Package paths provides shared filesystem path helpers for client and server.
package paths

import (
	"os"
	"path/filepath"
)

// AppDataDir returns the platform-specific application data directory.
//   - Windows: %APPDATA% (or %USERPROFILE%\AppData\Roaming)
//   - Linux/macOS: $HOME
//   - Fallback: "."
func AppDataDir() string {
	if appData := os.Getenv("APPDATA"); appData != "" {
		return appData
	}
	if userProfile := os.Getenv("USERPROFILE"); userProfile != "" {
		return filepath.Join(userProfile, "AppData", "Roaming")
	}
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	return "."
}

// GameTunnelDir returns the GameTunnel directory under AppData.
// Creates it if it doesn't exist.
func GameTunnelDir() string {
	dir := filepath.Join(AppDataDir(), "GameTunnel")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "."
	}
	return dir
}

// ExeDir returns the directory containing the running executable.
func ExeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}
