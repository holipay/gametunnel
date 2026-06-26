package main

import (
	"fmt"
	"log"
	"time"

	"github.com/getlantern/systray"

	"github.com/holipay/gametunnel/internal/i18n"
)

type Tray struct {
	app *App

	mStatus     *systray.MenuItem
	mConnect    *systray.MenuItem
	mDisconnect *systray.MenuItem
}

func RunTray(app *App) {
	systray.Run(func() {
		tray := &Tray{app: app}
		tray.setup()
	}, func() {
		app.Disconnect()
	})
}

func (tr *Tray) setup() {
	s := i18n.T()
	setTrayIcon(iconDisconnected)
	systray.SetTitle(s.TrayTitle)
	systray.SetTooltip(s.TrayTooltip)

	tr.mStatus = systray.AddMenuItem(s.TrayStatusOffline, "")
	tr.mStatus.Disable()

	systray.AddSeparator()

	tr.mConnect = systray.AddMenuItem(s.TrayConnect, s.TrayConnectDesc)
	tr.mDisconnect = systray.AddMenuItem(s.TrayDisconnect, s.TrayDisconnectDesc)
	tr.mDisconnect.Disable()

	systray.AddSeparator()

	mSettings := systray.AddMenuItem(s.TraySettings, s.TraySettingsDesc)
	mEditConfig := systray.AddMenuItem(s.TrayEditConfig, s.TrayEditConfigDesc)
	mLog := systray.AddMenuItem(s.TrayViewLog, s.TrayOpenLogFile)
	mQuit := systray.AddMenuItem(s.TrayQuit, s.TrayQuitDesc)

	// Wire up connection failure callback: open web UI for settings
	tr.app.Mu.Lock()
	tr.app.OnConnFailed = func(errMsg string) bool {
		log.Printf("connection failed: %s", errMsg)
		openBrowser("http://127.0.0.1:4702")
		return false // user will fix via web UI
	}
	tr.app.Mu.Unlock()

	// First run: open web UI to guide user
	isFirstRun := tr.app.Cfg.ServerAddr == ""
	if isFirstRun {
		go func() {
			time.Sleep(500 * time.Millisecond)
			showFirstRunNotify()
			openBrowser("http://127.0.0.1:4702")
		}()
	}

	go func() {
		for {
			select {
		case <-mSettings.ClickedCh:
			openBrowser("http://127.0.0.1:4702")

			case <-mEditConfig.ClickedCh:
				openConfigFile()

			case <-tr.mConnect.ClickedCh:
				go tr.doConnect()

			case <-tr.mDisconnect.ClickedCh:
				tr.app.Disconnect()
				tr.updateTray(false, "", 0, nil)

			case <-mLog.ClickedCh:
				openLogFile()

			case <-mQuit.ClickedCh:
				tr.app.Disconnect()
				systray.Quit()
				return
			}
		}
	}()

	go tr.statusLoop()
}

func (tr *Tray) doConnect() {
	// Snapshot cfg under lock to avoid data race with settings dialog
	tr.app.Mu.RLock()
	cfg := tr.app.Cfg
	tr.app.Mu.RUnlock()

	if cfg.ServerAddr == "" {
		openBrowser("http://127.0.0.1:4702")
		return
	}
	tr.app.Connect(cfg)
	tr.updateTrayConnecting()
}

func (tr *Tray) updateTrayConnecting() {
	s := i18n.T()
	setTrayIcon(iconConnecting)
	systray.SetTooltip(s.TrayTooltipConn)
	tr.mStatus.SetTitle(s.TrayConnecting)
	tr.mConnect.Disable()
	tr.mDisconnect.Enable()
}

func (tr *Tray) updateTray(connected bool, ip string, peers int, quality *StatusResponse) {
	s := i18n.T()
	if connected {
		// Choose icon based on connection type
		if quality != nil && quality.P2PPeers > 0 && quality.RelayPeers == 0 {
			setTrayIcon(iconConnectedP2P) // green: all peers are P2P
		} else if quality != nil && quality.RelayPeers > 0 {
			setTrayIcon(iconConnectedRelay) // yellow: using relay
		} else {
			setTrayIcon(iconConnected) // default green
		}

		// Build tooltip with connection quality info
		tooltip := fmt.Sprintf(s.TrayTooltipOnline, ip, peers)
		if quality != nil {
			if quality.P2PPeers > 0 || quality.RelayPeers > 0 {
				tooltip += fmt.Sprintf("\nP2P: %d  Relay: %d", quality.P2PPeers, quality.RelayPeers)
			}
			if quality.AvgRTT > 0 {
				tooltip += fmt.Sprintf("\nRTT: %.0fms", quality.AvgRTT)
			}
			if quality.LossRate > 0 {
				tooltip += fmt.Sprintf("\nLoss: %.0f%%", quality.LossRate*100)
			}
		}
		systray.SetTooltip(tooltip)

		// Status menu item
		statusText := fmt.Sprintf(s.TrayStatusOnline, ip, peers)
		if quality != nil && (quality.P2PPeers > 0 || quality.RelayPeers > 0) {
			statusText += fmt.Sprintf("  [P2P:%d Relay:%d]", quality.P2PPeers, quality.RelayPeers)
		}
		tr.mStatus.SetTitle(statusText)
		tr.mConnect.Disable()
		tr.mDisconnect.Enable()
	} else {
		setTrayIcon(iconDisconnected)
		systray.SetTooltip(s.TrayTooltip)
		tr.mStatus.SetTitle(s.TrayStatusOffline)
		tr.mConnect.Enable()
		tr.mDisconnect.Disable()
	}
}

func (tr *Tray) updateTrayError(errMsg string) {
	s := i18n.T()
	setTrayIcon(iconDisconnected)
	systray.SetTooltip(s.TrayStatusError)
	tr.mStatus.SetTitle(fmt.Sprintf("%s: %s", s.TrayStatusError, errMsg))
	tr.mConnect.Enable()
	tr.mDisconnect.Disable()
}

func (tr *Tray) statusLoop() {
	for {
		status := tr.app.GetStatus()
		if status.Connecting {
			tr.updateTrayConnecting()
		} else if status.Connected {
			tr.updateTray(true, status.VirtualIP, status.PeerCount, &status)
		} else if status.LastError != "" {
			tr.updateTrayError(status.LastError)
		} else {
			tr.updateTray(false, "", 0, nil)
		}

		// Snapshot ctx under lock to avoid race with App.Disconnect()
		tr.app.Mu.RLock()
		ctx := tr.app.Ctx
		tr.app.Mu.RUnlock()

		select {
		case <-ctx.Done():
			return
		default:
		}

		time.Sleep(2 * time.Second)
	}
}
