//go:build windows

package ui

import (
	"fmt"

	"github.com/lxn/walk"

	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/i18n"
)

// Tray manages the system tray icon and context menu.
type Tray struct {
	app *client.App
	mw  *walk.MainWindow

	// Menu items
	mStatus     *walk.Action
	mConnect    *walk.Action
	mDisconnect *walk.Action
	mSettings   *walk.Action
	mShowWindow *walk.Action
	mQuit       *walk.Action

	ni       *walk.NotifyIcon
	owner    walk.Form
	lastConn bool
}

// NewTray creates the system tray icon. owner is used as the parent window.
func NewTray(app *client.App, owner walk.Form) (*Tray, error) {
	ni, err := walk.NewNotifyIcon(owner)
	if err != nil {
		return nil, fmt.Errorf("create notify icon: %w", err)
	}

	t := &Tray{
		app:   app,
		ni:    ni,
		owner: owner,
	}

	t.setupMenu()
	t.updateIcon(false)
	ni.SetToolTip(i18n.T().DlgStatusIdle)

	// Left-click shows main window
	ni.MouseDown().Attach(func(x, y int, button walk.MouseButton) {
		if button == walk.LeftButton && t.mw != nil {
			t.mw.Show()
			t.mw.SetFocus()
		}
	})

	return t, nil
}

// SetMainWindow sets the main window reference for tray interactions.
func (t *Tray) SetMainWindow(mw *walk.MainWindow) {
	t.mw = mw
}

func (tr *Tray) setupMenu() {
	tt := i18n.T()

	tr.ni.SetIcon(walk.IconApplication())

	// Status (disabled, display only)
	tr.mStatus = walk.NewAction()
	tr.mStatus.SetText(tt.DlgStatusIdle)
	tr.mStatus.SetEnabled(false)
	tr.ni.ContextMenu().Actions().Add(tr.mStatus)

	tr.ni.ContextMenu().Actions().Add(walk.NewSeparatorAction())

	// Show window
	tr.mShowWindow = walk.NewAction()
	tr.mShowWindow.SetText(tt.DlgSettings)
	tr.mShowWindow.Triggered().Attach(func() {
		if tr.mw != nil {
			tr.mw.Show()
			tr.mw.SetFocus()
		}
	})
	tr.ni.ContextMenu().Actions().Add(tr.mShowWindow)

	// Connect
	tr.mConnect = walk.NewAction()
	tr.mConnect.SetText(tt.DlgConnect)
	tr.mConnect.Triggered().Attach(func() {
		tr.app.Connect(tr.app.Cfg)
	})
	tr.ni.ContextMenu().Actions().Add(tr.mConnect)

	// Disconnect
	tr.mDisconnect = walk.NewAction()
	tr.mDisconnect.SetText(tt.DlgDisconnect)
	tr.mDisconnect.SetEnabled(false)
	tr.mDisconnect.Triggered().Attach(func() {
		tr.app.Disconnect()
	})
	tr.ni.ContextMenu().Actions().Add(tr.mDisconnect)

	tr.ni.ContextMenu().Actions().Add(walk.NewSeparatorAction())

	// Settings
	tr.mSettings = walk.NewAction()
	tr.mSettings.SetText(tt.DlgSettings)
	tr.mSettings.Triggered().Attach(func() {
		cfg := ShowSettingsDialog(tr.owner, tr.app.Cfg)
		if cfg != nil {
			tr.app.Connect(cfg)
		}
	})
	tr.ni.ContextMenu().Actions().Add(tr.mSettings)

	tr.ni.ContextMenu().Actions().Add(walk.NewSeparatorAction())

	// Quit
	tr.mQuit = walk.NewAction()
	tr.mQuit.SetText("Exit")
	tr.mQuit.Triggered().Attach(func() {
		walk.App().Exit(0)
	})
	tr.ni.ContextMenu().Actions().Add(tr.mQuit)
}

// UpdateStatus updates the tray icon and menu based on connection status.
func (t *Tray) UpdateStatus(connected bool, peerCount int, virtualIP string) {
	if connected != t.lastConn {
		t.updateIcon(connected)
		t.lastConn = connected
	}

	t.mConnect.SetEnabled(!connected)
	t.mDisconnect.SetEnabled(connected)

	tt := i18n.T()
	var statusText, tooltip string
	if connected {
		statusText = fmt.Sprintf(tt.DlgStatusConn, virtualIP, peerCount)
		tooltip = fmt.Sprintf("GameTunnel - %s (%d)", tt.DlgConnect, peerCount)
	} else {
		statusText = tt.DlgStatusIdle
		tooltip = "GameTunnel - " + tt.DlgStatusIdle
	}
	t.mStatus.SetText(statusText)
	t.ni.SetToolTip(tooltip)

	// Show balloon on state change
	if connected {
		t.ni.ShowMessage("GameTunnel", fmt.Sprintf("Connected\nVirtual IP: %s", virtualIP))
	}
}

func (t *Tray) updateIcon(connected bool) {
	if connected {
		t.ni.SetIcon(walk.IconInformation())
	} else {
		t.ni.SetIcon(walk.IconApplication())
	}
}

// Dispose cleans up the tray icon.
func (t *Tray) Dispose() {
	if t.ni != nil {
		t.ni.Dispose()
	}
}
