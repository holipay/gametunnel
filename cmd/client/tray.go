package main

import (
	"fmt"
	"log"
	"time"

	"github.com/getlantern/systray"

	"github.com/holipay/gametunnel/internal/client"
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

	// Wire up connection failure callback: show error dialog after fast retries
	tr.app.onConnFailed = func(errMsg string) bool {
		tr.app.dialogMu.Lock()
		defer tr.app.dialogMu.Unlock()
		return showConnErrorDialog(errMsg)
	}

	// First run: auto-open settings dialog to guide user
	isFirstRun := tr.app.cfg.ServerAddr == ""
	if isFirstRun {
		go func() {
			time.Sleep(500 * time.Millisecond)
			// Show notification so user can find the tray icon
			showFirstRunNotify()
			statusText := s.TrayNoServer
			if showSettingsDialog(statusText) {
				cfg := client.LoadConfig()
				tr.app.mu.Lock()
				tr.app.cfg = cfg
				tr.app.mu.Unlock()
				if cfg.Lang != "" {
					i18n.Set(i18n.ParseLang(cfg.Lang))
				}
				if cfg.ServerAddr != "" {
					tr.app.Connect(cfg)
				}
			}
		}()
	}

	go func() {
		for {
			select {
			case <-mSettings.ClickedCh:
				go func() {
					tr.app.dialogMu.Lock()
					defer tr.app.dialogMu.Unlock()
					status := tr.app.GetStatus()
					statusText := i18n.T().DlgStatusIdle
					if status.Connected {
						statusText = fmt.Sprintf(i18n.T().DlgStatusConn, status.VirtualIP, status.PeerCount)
					}
				if showSettingsDialog(statusText) {
					cfg := client.LoadConfig()
					tr.app.mu.Lock()
					tr.app.cfg = cfg
					tr.app.mu.Unlock()
					if cfg.Lang != "" {
							i18n.Set(i18n.ParseLang(cfg.Lang))
						}
						log.Printf("%s", i18n.T().TrayCfgUpdated)
					}
				}()

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
	if tr.app.cfg.ServerAddr == "" {
		tr.app.dialogMu.Lock()
		defer tr.app.dialogMu.Unlock()
		statusText := i18n.T().TrayNoServer
		if showSettingsDialog(statusText) {
			cfg := client.LoadConfig()
			tr.app.mu.Lock()
			tr.app.cfg = cfg
			tr.app.mu.Unlock()
			if cfg.Lang != "" {
				i18n.Set(i18n.ParseLang(cfg.Lang))
			}
			if cfg.ServerAddr != "" {
				tr.app.Connect(cfg)
			}
		}
		return
	}
	tr.app.Connect(tr.app.cfg)
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

		select {
		case <-tr.app.ctx.Done():
			return
		default:
		}

		time.Sleep(2 * time.Second)
	}
}
