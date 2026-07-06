//go:build windows

package ui

import (
	"fmt"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"

	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/i18n"
)

// StatusWindow is the main status window showing connection info.
type StatusWindow struct {
	window *walk.MainWindow

	app  *client.App
	tray *Tray
	ticker *time.Ticker
	stopCh chan struct{}

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
				Title:  i18n.T().UIStatus,
				Layout: Grid{Columns: 2, Spacing: 6, Margins: Margins{Left: 10, Top: 10, Right: 10, Bottom: 10}},
				Children: []Widget{
					Label{Text: i18n.T().UIStatusTitle + ":"},
					Label{AssignTo: &sw.statusLabel, Text: i18n.T().DlgStatusIdle},

					Label{Text: i18n.T().UIVirtualIP + ":"},
					Label{AssignTo: &sw.vipLabel, Text: "-"},

					Label{Text: i18n.T().UIRoom + ":"},
					Label{AssignTo: &sw.roomLabel, Text: "-"},

					Label{Text: i18n.T().UIOnlinePlayers + ":"},
					Label{AssignTo: &sw.playerLabel, Text: "0"},

					Label{Text: i18n.T().UIUptime + ":"},
					Label{AssignTo: &sw.uptimeLabel, Text: "-"},

					Label{Text: i18n.T().UIQuality + ":"},
					Label{AssignTo: &sw.qualityLabel, Text: "-"},
				},
			},
			Composite{
				Layout: HBox{Spacing: 6},
				Children: []Widget{
					PushButton{
						Text: i18n.T().DlgConnect,
						OnClicked: func() {
							app.Connect(app.Cfg)
						},
					},
					PushButton{
						Text: i18n.T().DlgDisconnect,
						OnClicked: func() {
							app.Disconnect()
						},
					},
					PushButton{
						Text: i18n.T().DlgSettings,
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
	sw.stopCh = make(chan struct{})
	sw.ticker = time.NewTicker(time.Second)
	go func() {
		for {
			select {
			case <-sw.ticker.C:
				sw.refresh()
			case <-sw.stopCh:
				return
			}
		}
	}()
}

func (sw *StatusWindow) refresh() {
	if sw.window == nil || sw.window.IsDisposed() {
		return
	}

	status := sw.app.GetStatus()

	sw.window.Synchronize(func() {
		if status.Connected {
			sw.statusLabel.SetText(i18n.T().DlgStatusConn)
			sw.vipLabel.SetText(status.VirtualIP)
			sw.roomLabel.SetText(status.RoomID)
			sw.playerLabel.SetText(fmt.Sprintf("%d", status.PeerCount))
			sw.uptimeLabel.SetText(status.Uptime)
			if status.AvgRTT > 0 {
				sw.qualityLabel.SetText(fmt.Sprintf(i18n.T().UIQualityFmt,
					status.AvgRTT, status.LossRate*100))
			} else {
				sw.qualityLabel.SetText("-")
			}
		} else if status.Connecting {
			sw.statusLabel.SetText(i18n.T().UIConnecting)
			sw.vipLabel.SetText("-")
			sw.roomLabel.SetText(status.RoomID)
			sw.playerLabel.SetText("0")
			sw.uptimeLabel.SetText("-")
			sw.qualityLabel.SetText("-")
			if status.LastError != "" {
				sw.statusLabel.SetText(status.LastError)
			}
		} else {
			sw.statusLabel.SetText(i18n.T().DlgStatusIdle)
			sw.vipLabel.SetText("-")
			sw.roomLabel.SetText("-")
			sw.playerLabel.SetText("0")
			sw.uptimeLabel.SetText("-")
			sw.qualityLabel.SetText("-")
		}

		sw.tray.UpdateStatus(status.Connected, status.PeerCount, status.VirtualIP)
	})
}

// Dispose cleans up the main window.
func (sw *StatusWindow) Dispose() {
	if sw.ticker != nil {
		sw.ticker.Stop()
	}
	if sw.stopCh != nil {
		close(sw.stopCh)
	}
	if sw.window != nil {
		sw.window.Dispose()
	}
}
