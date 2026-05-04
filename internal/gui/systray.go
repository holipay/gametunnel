package gui

import (
	"fmt"
	"log"
	"sync"

	"github.com/getlantern/systray"
	"github.com/holipay/gametunnel/internal/util"
)

// State represents the tunnel connection state.
type State int

const (
	StateDisconnected State = iota
	StateConnecting
	StateConnected
)

// GUI manages the system tray interface.
type GUI struct {
	cfg    *Config
	state  State
	ip     string // assigned virtual IP
	mu     sync.Mutex

	// Menu items
	mStatus   *systray.MenuItem
	mIP       *systray.MenuItem
	mPlayers  *systray.MenuItem
	mNotice   *systray.MenuItem
	mConnect  *systray.MenuItem
	mSettings *systray.MenuItem

	// Callbacks
	onConnect    func()
	onDisconnect func()
}

// New creates a new GUI instance.
func New(cfg *Config) *GUI {
	return &GUI{cfg: cfg}
}

// SetCallbacks registers connect/disconnect handlers.
func (g *GUI) SetCallbacks(onConnect, onDisconnect func()) {
	g.onConnect = onConnect
	g.onDisconnect = onDisconnect
}

// Run starts the system tray. Blocks until quit is clicked.
func (g *GUI) Run() {
	systray.Run(g.onReady, g.onExit)
}

// UpdateState updates the tray icon and status text.
func (g *GUI) UpdateState(state State) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.state = state

	switch state {
	case StateDisconnected:
		systray.SetIcon(iconRed)
		systray.SetTooltip("GameTunnel - 未连接")
		g.mStatus.SetTitle("状态: 未连接")
		g.mIP.Hide()
		g.mPlayers.Hide()
		g.mConnect.SetTitle("连接")
	case StateConnecting:
		systray.SetIcon(iconRed)
		systray.SetTooltip("GameTunnel - 连接中...")
		g.mStatus.SetTitle("状态: 连接中...")
		g.mNotice.Hide() // clear first-run notice
		g.mConnect.SetTitle("取消")
	case StateConnected:
		systray.SetIcon(iconGreen)
		systray.SetTooltip(fmt.Sprintf("GameTunnel - 已连接 (%s)", g.ip))
		g.mStatus.SetTitle("状态: ✅ 已连接")
		g.mIP.SetTitle(fmt.Sprintf("虚拟IP: %s", g.ip))
		g.mIP.Show()
		g.mPlayers.Show()
		g.mNotice.Hide()
		g.mConnect.SetTitle("断开")
	}
}

// SetIP sets the assigned virtual IP (called after registration).
func (g *GUI) SetIP(ip string) {
	g.mu.Lock()
	g.ip = ip
	g.mu.Unlock()
}

// SetPlayers updates the online player count.
func (g *GUI) SetPlayers(count int) {
	g.mPlayers.SetTitle(fmt.Sprintf("在线玩家: %d", count+1)) // +1 for self
}

// SetNotice shows a one-time notice in the menu bar (e.g. first-run hint).
func (g *GUI) SetNotice(text string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.mNotice != nil {
		g.mNotice.SetTitle(text)
		g.mNotice.Show()
	}
}

func (g *GUI) onReady() {
	systray.SetIcon(iconRed)
	systray.SetTitle("GameTunnel")
	systray.SetTooltip("GameTunnel - 局域网游戏隧道")

	// Status section (non-clickable, just display)
	g.mStatus = systray.AddMenuItem("状态: 未连接", "连接状态")
	g.mStatus.Disable()
	g.mIP = systray.AddMenuItem("", "虚拟IP")
	g.mIP.Disable()
	g.mIP.Hide()
	g.mPlayers = systray.AddMenuItem("", "在线玩家数")
	g.mPlayers.Disable()
	g.mPlayers.Hide()
	g.mNotice = systray.AddMenuItem("", "通知")
	g.mNotice.Disable()
	g.mNotice.Hide()

	systray.AddSeparator()

	// Actions
	g.mConnect = systray.AddMenuItem("连接", "连接/断开隧道")
	g.mSettings = systray.AddMenuItem("设置", "打开配置文件")

	systray.AddSeparator()

	// Quit
	mQuit := systray.AddMenuItem("退出", "退出 GameTunnel")

	// Handle menu clicks
	go func() {
		for {
			select {
			case <-g.mConnect.ClickedCh:
				g.mu.Lock()
				state := g.state
				g.mu.Unlock()
				if state == StateDisconnected {
					if g.onConnect != nil {
						go g.onConnect()
					}
				} else {
					if g.onDisconnect != nil {
						go g.onDisconnect()
					}
				}
			case <-g.mSettings.ClickedCh:
				g.openSettings()
			case <-mQuit.ClickedCh:
				if g.onDisconnect != nil {
					g.onDisconnect()
				}
				systray.Quit()
				return
			}
		}
	}()

	// Auto-connect if configured
	if g.cfg.AutoConnect && g.cfg.ServerAddr != "" {
		if g.onConnect != nil {
			go g.onConnect()
		}
	}
}

func (g *GUI) onExit() {
	// Cleanup
}

// openSettings opens the config file in notepad.
func (g *GUI) openSettings() {
	path := ConfigPath()
	// Ensure file exists
	SaveConfig(g.cfg)

	// Non-blocking: launch notepad, reload config when it closes
	go func() {
		if err := util.RunCmd("notepad.exe", path); err != nil {
			log.Printf("[gui] 打开设置失败: %v", err)
			return
		}
		newCfg := LoadConfig()
		g.mu.Lock()
		g.cfg = newCfg
		g.mu.Unlock()
	}()
}
