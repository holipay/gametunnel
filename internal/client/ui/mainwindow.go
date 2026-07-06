//go:build windows

package ui

import (
	"fmt"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"

	"github.com/holipay/gametunnel/internal/client"
)

// StatusWindow is the main status window showing connection info.
type StatusWindow struct {
	window *walk.MainWindow

	app  *client.App
	tray *Tray
	timer *time.Timer

	// UI elements
	statusLabel  *walk.Label
	vipLabel     *walk.Label
	roomLabel    *walk.Label
	playerLabel  *walk.Label
	uptimeLabel  *walk.Label
	qualityLabel *walk.Label
}

// NewStatusWindow creates the main status window.
func NewStatusWindow(app *client.App, tray *Tray, owner walk.Form) (*StatusWindow, error) {
	sw := &StatusWindow{
		app:  app,
		tray: tray,
	}

	err := MainWindow{
		AssignTo: &sw.window,
		Title:    "GameTunnel",
		MinSize:  Size{Width: 400, Height: 300},
		Size:     Size{Width: 450, Height: 350},
		Layout:   VBox{Margins: Margins{Left: 10, Top: 10, Right: 10, Bottom: 10}, Spacing: 8},
		Children: []Widget{
			GroupBox{
				Title:  "连接状态",
				Layout: Grid{Columns: 2, Spacing: 6, Margins: Margins{Left: 10, Top: 10, Right: 10, Bottom: 10}},
				Children: []Widget{
					Label{Text: "状态:"},
					Label{AssignTo: &sw.statusLabel, Text: "未连接"},

					Label{Text: "虚拟 IP:"},
					Label{AssignTo: &sw.vipLabel, Text: "-"},

					Label{Text: "房间:"},
					Label{AssignTo: &sw.roomLabel, Text: "-"},

					Label{Text: "在线玩家:"},
					Label{AssignTo: &sw.playerLabel, Text: "0"},

					Label{Text: "运行时间:"},
					Label{AssignTo: &sw.uptimeLabel, Text: "-"},

					Label{Text: "质量:"},
					Label{AssignTo: &sw.qualityLabel, Text: "-"},
				},
			},
			Composite{
				Layout: HBox{Spacing: 6},
				Children: []Widget{
					PushButton{
						Text: "连接",
						OnClicked: func() {
							app.Connect(app.Cfg)
						},
					},
					PushButton{
						Text: "断开",
						OnClicked: func() {
							app.Disconnect()
						},
					},
					PushButton{
						Text: "设置...",
						OnClicked: func() {
							cfg := ShowSettingsDialog(sw.window, app.Cfg)
							if cfg != nil {
								app.Connect(cfg)
							}
						},
					},
				},
			},
		},
	}.Create()

	if err != nil {
		return nil, err
	}

	// Hide window on close (minimize to tray)
	sw.window.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		*canceled = true
		sw.window.Hide()
	})

	// Start periodic UI refresh
	sw.startRefresh()

	return sw, nil
}

// Window returns the underlying walk.MainWindow for use as dialog owner.
func (sw *StatusWindow) Window() *walk.MainWindow {
	return sw.window
}

// Show shows the main window.
func (sw *StatusWindow) Show() {
	if sw.window != nil {
		sw.window.Show()
	}
}

// SetFocus brings the main window to front.
func (sw *StatusWindow) SetFocus() {
	if sw.window != nil {
		sw.window.SetFocus()
	}
}

func (sw *StatusWindow) startRefresh() {
	sw.timer = time.AfterFunc(time.Second, sw.refresh)
}

func (sw *StatusWindow) refresh() {
	if sw.window == nil || sw.window.IsDisposed() {
		return
	}

	status := sw.app.GetStatus()

	sw.window.Synchronize(func() {
		if status.Connected {
			sw.statusLabel.SetText("已连接")
			sw.vipLabel.SetText(status.VirtualIP)
			sw.roomLabel.SetText(status.RoomID)
			sw.playerLabel.SetText(fmt.Sprintf("%d", status.PeerCount))
			sw.uptimeLabel.SetText(status.Uptime)
			if status.AvgRTT > 0 {
				sw.qualityLabel.SetText(fmt.Sprintf("延迟 %.0fms · 丢包 %.0f%%",
					status.AvgRTT, status.LossRate*100))
			} else {
				sw.qualityLabel.SetText("-")
			}
		} else if status.Connecting {
			sw.statusLabel.SetText("连接中...")
			sw.vipLabel.SetText("-")
			sw.roomLabel.SetText(status.RoomID)
			sw.playerLabel.SetText("0")
			sw.uptimeLabel.SetText("-")
			sw.qualityLabel.SetText("-")
			if status.LastError != "" {
				sw.statusLabel.SetText("连接失败: " + status.LastError)
			}
		} else {
			sw.statusLabel.SetText("未连接")
			sw.vipLabel.SetText("-")
			sw.roomLabel.SetText("-")
			sw.playerLabel.SetText("0")
			sw.uptimeLabel.SetText("-")
			sw.qualityLabel.SetText("-")
		}

		sw.tray.UpdateStatus(status.Connected, status.PeerCount, status.VirtualIP)
	})

	sw.timer = time.AfterFunc(time.Second, sw.refresh)
}

// Dispose cleans up the main window.
func (sw *StatusWindow) Dispose() {
	if sw.timer != nil {
		sw.timer.Stop()
	}
	if sw.window != nil {
		sw.window.Dispose()
	}
}
