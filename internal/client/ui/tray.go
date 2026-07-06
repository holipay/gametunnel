//go:build windows

package ui

import (
	"fmt"
	"syscall"

	"github.com/lxn/walk"

	"github.com/holipay/gametunnel/internal/client"
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
	ni.SetToolTip("GameTunnel - 未连接")

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

func (t *Tray) setupMenu() {
	t.ni.SetIcon(walk.IconApplication())

	// Status (disabled, display only)
	t.mStatus = walk.NewAction()
	t.mStatus.SetText("未连接")
	t.mStatus.SetEnabled(false)
	t.ni.ContextMenu().Actions().Add(t.mStatus)

	t.ni.ContextMenu().Actions().Add(walk.NewSeparatorAction())

	// Show window
	t.mShowWindow = walk.NewAction()
	t.mShowWindow.SetText("打开主窗口")
	t.mShowWindow.Triggered().Attach(func() {
		if t.mw != nil {
			t.mw.Show()
			t.mw.SetFocus()
		}
	})
	t.ni.ContextMenu().Actions().Add(t.mShowWindow)

	// Connect
	t.mConnect = walk.NewAction()
	t.mConnect.SetText("连接")
	t.mConnect.Triggered().Attach(func() {
		t.app.Connect(t.app.Cfg)
	})
	t.ni.ContextMenu().Actions().Add(t.mConnect)

	// Disconnect
	t.mDisconnect = walk.NewAction()
	t.mDisconnect.SetText("断开连接")
	t.mDisconnect.SetEnabled(false)
	t.mDisconnect.Triggered().Attach(func() {
		t.app.Disconnect()
	})
	t.ni.ContextMenu().Actions().Add(t.mDisconnect)

	t.ni.ContextMenu().Actions().Add(walk.NewSeparatorAction())

	// Settings
	t.mSettings = walk.NewAction()
	t.mSettings.SetText("设置...")
	t.mSettings.Triggered().Attach(func() {
		cfg := ShowSettingsDialog(t.owner, t.app.Cfg)
		if cfg != nil {
			t.app.Connect(cfg)
		}
	})
	t.ni.ContextMenu().Actions().Add(t.mSettings)

	t.ni.ContextMenu().Actions().Add(walk.NewSeparatorAction())

	// Quit
	t.mQuit = walk.NewAction()
	t.mQuit.SetText("退出")
	t.mQuit.Triggered().Attach(func() {
		walk.App().Exit(0)
	})
	t.ni.ContextMenu().Actions().Add(t.mQuit)
}

// UpdateStatus updates the tray icon and menu based on connection status.
func (t *Tray) UpdateStatus(connected bool, peerCount int, virtualIP string) {
	if connected != t.lastConn {
		t.updateIcon(connected)
		t.lastConn = connected
	}

	t.mConnect.SetEnabled(!connected)
	t.mDisconnect.SetEnabled(connected)

	var statusText, tooltip string
	if connected {
		statusText = fmt.Sprintf("已连接 · %s · %d人在线", virtualIP, peerCount)
		tooltip = fmt.Sprintf("GameTunnel - 已连接 (%d人)", peerCount)
	} else {
		statusText = "未连接"
		tooltip = "GameTunnel - 未连接"
	}
	t.mStatus.SetText(statusText)
	t.ni.SetToolTip(tooltip)

	// Show balloon on state change
	if connected {
		t.ni.ShowMessage("GameTunnel", fmt.Sprintf("已连接到服务器\n虚拟 IP: %s", virtualIP))
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

// HideConsole hides the console window on Windows.
func HideConsole() {
	user32 := syscall.NewLazyDLL("user32.dll")
	kernel32 := syscall.NewLazyDLL("kernel32.dll")

	hwnd, _, _ := kernel32.NewProc("GetConsoleWindow").Call()
	if hwnd != 0 {
		user32.NewProc("ShowWindow").Call(hwnd, 0) // SW_HIDE
	}
}
