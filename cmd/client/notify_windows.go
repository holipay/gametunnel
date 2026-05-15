//go:build windows

package main

import (
	"golang.org/x/sys/windows"

	"github.com/holipay/gametunnel/internal/i18n"
)

// showFirstRunNotify shows a notification to help the user find the tray icon.
// Uses a simple MessageBox since getlantern/systray doesn't support balloons.
func showFirstRunNotify() {
	s := i18n.T()
	windows.MessageBox(0,
		windows.StringToUTF16Ptr(s.FirstRunBalloon),
		windows.StringToUTF16Ptr(s.ConnErrBalloonTitle),
		windows.MB_OK|windows.MB_ICONINFORMATION)
}
