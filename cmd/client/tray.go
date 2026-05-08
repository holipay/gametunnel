package main

import (
	"fmt"
	"log"
	"os/exec"
	"runtime"
	"time"

	"github.com/getlantern/systray"
)

// Tray manages the system tray icon and menu.
type Tray struct {
	app    *App
	httpSrv *HTTPServer

	// Menu items (updated dynamically)
	mStatus    *systray.MenuItem
	mConnect   *systray.MenuItem
	mDisconnect *systray.MenuItem
	mDashboard *systray.MenuItem
}

// RunTray starts the system tray. Blocks until quit.
func RunTray(app *App, httpSrv *HTTPServer) {
	systray.Run(func() {
		tray := &Tray{app: app, httpSrv: httpSrv}
		tray.setup()
	}, func() {
		app.Disconnect()
		httpSrv.Stop()
	})
}

func (t *Tray) setup() {
	systray.SetIcon(iconDisconnected)
	systray.SetTitle("GameTunnel")
	systray.SetTooltip("GameTunnel - 未连接")

	// Status display (disabled, just for showing info)
	t.mStatus = systray.AddMenuItem("🔴 未连接", "")
	t.mStatus.Disable()

	systray.AddSeparator()

	// Actions
	t.mDashboard = systray.AddMenuItem("📊 打开面板", "打开 Web 控制面板")
	t.mConnect = systray.AddMenuItem("⚡ 一键加入", "连接到服务器")
	t.mDisconnect = systray.AddMenuItem("🔌 断开连接", "断开当前连接")
	t.mDisconnect.Disable()

	systray.AddSeparator()

	// Utility
	mLog := systray.AddMenuItem("📄 查看日志", "打开日志文件")
	mQuit := systray.AddMenuItem("❌ 退出", "退出 GameTunnel")

	// Event loop
	go func() {
		for {
			select {
			case <-t.mDashboard.ClickedCh:
				openBrowser(fmt.Sprintf("http://127.0.0.1%s", t.httpSrv.addr))

			case <-t.mConnect.ClickedCh:
				go t.doConnect()

			case <-t.mDisconnect.ClickedCh:
				t.app.Disconnect()
				t.updateTray(false, "", 0, 0)

			case <-mLog.ClickedCh:
				openLogFile()

			case <-mQuit.ClickedCh:
				t.app.Disconnect()
				t.httpSrv.Stop()
				systray.Quit()
				return
			}
		}
	}()

	// Periodic tray status update
	go t.statusLoop()
}

func (t *Tray) doConnect() {
	if t.app.cfg.ServerAddr == "" {
		openBrowser(fmt.Sprintf("http://127.0.0.1%s", t.httpSrv.addr))
		return
	}
	t.app.Connect(t.app.cfg)
	t.updateTrayConnecting()
}

func (t *Tray) updateTrayConnecting() {
	systray.SetIcon(iconConnecting)
	systray.SetTooltip("GameTunnel - 连接中...")
	t.mStatus.SetTitle("🟡 连接中...")
	t.mConnect.Disable()
	t.mDisconnect.Enable()
}

func (t *Tray) updateTray(connected bool, ip string, peers int, rttMs int64) {
	if connected {
		systray.SetIcon(iconConnected)
		tooltip := fmt.Sprintf("GameTunnel - %s · %d人在线", ip, peers)
		if rttMs > 0 {
			tooltip += fmt.Sprintf(" · %dms", rttMs)
		}
		systray.SetTooltip(tooltip)
		t.mStatus.SetTitle(fmt.Sprintf("🟢 %s · %d人", ip, peers))
		t.mConnect.Disable()
		t.mDisconnect.Enable()
	} else {
		systray.SetIcon(iconDisconnected)
		systray.SetTooltip("GameTunnel - 未连接")
		t.mStatus.SetTitle("🔴 未连接")
		t.mConnect.Enable()
		t.mDisconnect.Disable()
	}
}

// statusLoop polls the app status and updates the tray icon.
func (t *Tray) statusLoop() {
	for {
		status := t.app.GetStatus()
		if status.Connecting {
			t.updateTrayConnecting()
		} else {
			t.updateTray(status.Connected, status.VirtualIP, len(status.Peers), status.RTT)
		}

		select {
		case <-t.app.ctx.Done():
			return
		default:
		}

		time.Sleep(2 * time.Second)
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("[tray] 打开浏览器失败: %v", err)
	}
}
