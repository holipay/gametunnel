//go:build darwin

package main

import "github.com/getlantern/systray"

// setTrayIcon sets the tray icon, using SetTemplateIcon on macOS
// for proper dark/light mode support.
func setTrayIcon(iconBytes []byte) {
	systray.SetTemplateIcon(iconBytes, iconBytes)
}
