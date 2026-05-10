//go:build !darwin

package main

import "github.com/getlantern/systray"

// setTrayIcon sets the tray icon on Windows and Linux.
func setTrayIcon(iconBytes []byte) {
	systray.SetIcon(iconBytes)
}
