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
	mLog := systray.AddMenuItem(s.TrayViewLog, s.TrayOpenLogFile)
	mQuit := systray.AddMenuItem(s.TrayQuit, s.TrayQuitDesc)

	go func() {
		for {
			select {
			case <-mSettings.ClickedCh:
				go func() {
					status := tr.app.GetStatus()
					statusText := i18n.T().DlgStatusIdle
					if status.Connected {
						statusText = fmt.Sprintf(i18n.T().DlgStatusConn, status.VirtualIP, status.PeerCount)
					}
					if showSettingsDialog(statusText) {
						cfg := client.LoadConfig()
						tr.app.cfg = cfg
						if cfg.Lang != "" {
							i18n.Set(i18n.ParseLang(cfg.Lang))
						}
						log.Printf(i18n.T().TrayCfgUpdated)
					}
				}()

			case <-tr.mConnect.ClickedCh:
				go tr.doConnect()

			case <-tr.mDisconnect.ClickedCh:
				tr.app.Disconnect()
				tr.updateTray(false, "", 0)

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
		statusText := i18n.T().TrayNoServer
		if showSettingsDialog(statusText) {
			cfg := client.LoadConfig()
			tr.app.cfg = cfg
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

func (tr *Tray) updateTray(connected bool, ip string, peers int) {
	s := i18n.T()
	if connected {
		setTrayIcon(iconConnected)
		systray.SetTooltip(fmt.Sprintf(s.TrayTooltipOnline, ip, peers))
		tr.mStatus.SetTitle(fmt.Sprintf(s.TrayStatusOnline, ip, peers))
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

func (tr *Tray) statusLoop() {
	for {
		status := tr.app.GetStatus()
		if status.Connecting {
			tr.updateTrayConnecting()
		} else {
			tr.updateTray(status.Connected, status.VirtualIP, status.PeerCount)
		}

		select {
		case <-tr.app.ctx.Done():
			return
		default:
		}

		time.Sleep(2 * time.Second)
	}
}
