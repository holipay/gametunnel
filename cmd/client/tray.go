package main

import (
	"fmt"
	"log"
	"time"

	"github.com/getlantern/systray"

	"github.com/holipay/gametunnel/internal/client"
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

func (t *Tray) setup() {
	setTrayIcon(iconDisconnected)
	systray.SetTitle("GameTunnel")
	systray.SetTooltip("GameTunnel - 未连接")

	t.mStatus = systray.AddMenuItem("🔴 未连接", "")
	t.mStatus.Disable()

	systray.AddSeparator()

	t.mConnect = systray.AddMenuItem("⚡ 连接", "连接到服务器")
	t.mDisconnect = systray.AddMenuItem("🔌 断开", "断开当前连接")
	t.mDisconnect.Disable()

	systray.AddSeparator()

	mSettings := systray.AddMenuItem("⚙ 设置...", "配置服务器和玩家信息")
	mLog := systray.AddMenuItem("📄 查看日志", "打开日志文件")
	mQuit := systray.AddMenuItem("❌ 退出", "退出 GameTunnel")

	go func() {
		for {
			select {
			case <-mSettings.ClickedCh:
				go func() {
					status := t.app.GetStatus()
					statusText := "未连接"
					if status.Connected {
						statusText = fmt.Sprintf("已连接 · %s · %d人在线", status.VirtualIP, status.PeerCount)
					}
					if showConfigDialog(statusText) {
						// Config changed, reload
						cfg := client.LoadConfig()
						t.app.cfg = cfg
						log.Printf("[tray] 配置已更新")
					} else {
						// Dialog failed or user cancelled — open config.ini as fallback
						openConfigFile()
					}
				}()

			case <-t.mConnect.ClickedCh:
				go t.doConnect()

			case <-t.mDisconnect.ClickedCh:
				t.app.Disconnect()
				t.updateTray(false, "", 0)

			case <-mLog.ClickedCh:
				openLogFile()

			case <-mQuit.ClickedCh:
				t.app.Disconnect()
				systray.Quit()
				return
			}
		}
	}()

	go t.statusLoop()
}

func (t *Tray) doConnect() {
	if t.app.cfg.ServerAddr == "" {
		// No server configured, show settings
		statusText := "请先配置服务器地址"
		if showConfigDialog(statusText) {
			cfg := client.LoadConfig()
			t.app.cfg = cfg
			if cfg.ServerAddr != "" {
				t.app.Connect(cfg)
			}
		}
		return
	}
	t.app.Connect(t.app.cfg)
	t.updateTrayConnecting()
}

func (t *Tray) updateTrayConnecting() {
	setTrayIcon(iconConnecting)
	systray.SetTooltip("GameTunnel - 连接中...")
	t.mStatus.SetTitle("🟡 连接中...")
	t.mConnect.Disable()
	t.mDisconnect.Enable()
}

func (t *Tray) updateTray(connected bool, ip string, peers int) {
	if connected {
		setTrayIcon(iconConnected)
		systray.SetTooltip(fmt.Sprintf("GameTunnel - %s · %d人在线", ip, peers))
		t.mStatus.SetTitle(fmt.Sprintf("🟢 %s · %d人", ip, peers))
		t.mConnect.Disable()
		t.mDisconnect.Enable()
	} else {
		setTrayIcon(iconDisconnected)
		systray.SetTooltip("GameTunnel - 未连接")
		t.mStatus.SetTitle("🔴 未连接")
		t.mConnect.Enable()
		t.mDisconnect.Disable()
	}
}

func (t *Tray) statusLoop() {
	for {
		status := t.app.GetStatus()
		if status.Connecting {
			t.updateTrayConnecting()
		} else {
			t.updateTray(status.Connected, status.VirtualIP, status.PeerCount)
		}

		select {
		case <-t.app.ctx.Done():
			return
		default:
		}

		time.Sleep(2 * time.Second)
	}
}

