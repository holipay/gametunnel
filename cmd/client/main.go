//go:build windows

// GameTunnel Client — 通用局域网游戏隧道 (Windows)
//
// Usage:
//
//	gtunnel-client.exe
//	gtunnel-client.exe -server 1.2.3.4:4700
//	gtunnel-client.exe -server 1.2.3.4:4700 -name Player1 -room myroom -password secret
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/tun"
)

// Version is set at build time via -ldflags.
var Version = "dev"

func main() {
	// Set console to UTF-8
	windows.SetConsoleOutputCP(65001)

	// Request admin rights (UAC prompt) if not elevated
	requestAdmin()

	// Run the tunnel
	err := run()

	// Pause before exit so double-click users can see output
	fmt.Print("\n  按回车键退出...")
	bufio.NewReader(os.Stdin).ReadBytes('\n')

	if err != nil {
		os.Exit(1)
	}
}

// requestAdmin checks if the process is running with admin rights.
// If not, it re-launches itself with the "runas" verb (UAC prompt)
// and exits the current non-elevated process.
func requestAdmin() {
	token := windows.GetCurrentProcessToken()
	if token.IsElevated() {
		return
	}

	exe, err := os.Executable()
	if err != nil {
		return
	}

	verb, _ := windows.UTF16PtrFromString("runas")
	exePath, _ := windows.UTF16PtrFromString(exe)

	if err := windows.ShellExecute(0, verb, exePath, nil, nil, windows.SW_SHOW); err != nil {
		fmt.Fprintf(os.Stderr, "  需要管理员权限运行\n")
		os.Exit(1)
	}

	// Exit the non-elevated process; the elevated one is now running
	os.Exit(0)
}

// run contains the main application logic. Returns an error on failure.
func run() error {
	serverFlag := flag.String("server", "", "服务器地址 (host:port)")
	nameFlag := flag.String("name", "", "玩家名称")
	roomFlag := flag.String("room", "", "房间ID")
	passFlag := flag.String("password", "", "房间密码")
	mtuFlag := flag.Int("mtu", 1400, "隧道 MTU")
	versionFlag := flag.Bool("version", false, "显示版本")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("gtunnel-client %s\n", Version)
		return nil
	}

	// Load config (config.ini next to exe > AppData/config.json > defaults)
	cfg := client.LoadConfig()
	if *serverFlag != "" {
		cfg.ServerAddr = *serverFlag
	}
	if *nameFlag != "" {
		cfg.PlayerName = *nameFlag
	}
	if *roomFlag != "" {
		cfg.RoomID = *roomFlag
	}
	if *passFlag != "" {
		cfg.RoomPassword = *passFlag
	}

	// No server configured — create default config.ini and guide user
	if cfg.ServerAddr == "" {
		path := client.CreateDefaultConfig()
		fmt.Fprintf(os.Stderr, "  首次运行，已创建配置文件:\n")
		fmt.Fprintf(os.Stderr, "  %s\n\n", path)
		fmt.Fprintf(os.Stderr, "  请用记事本编辑此文件，填入服务器地址后重新运行。\n")
		fmt.Fprintf(os.Stderr, "  或使用命令行: gtunnel-client.exe -server 你的服务器IP:4700\n\n")
		return fmt.Errorf("未配置服务器地址")
	}

	// Save config so subsequent runs remember CLI overrides
	client.SaveConfig(cfg)

	// Setup logging (file + stderr)
	logFile := client.SetupLog()
	defer logFile.Close()

	// Graceful shutdown on Ctrl+C
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	t := client.New(cfg)

	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\n  正在断开...")
		cancel()
		t.Disconnect()
	}()

	fmt.Printf("  GameTunnel 客户端 %s\n", Version)
	fmt.Printf("  服务器: %s\n", cfg.ServerAddr)
	fmt.Printf("  玩家:   %s\n", cfg.PlayerName)
	fmt.Printf("  房间:   %s\n", cfg.RoomID)
	if cfg.RoomPassword != "" {
		fmt.Printf("  认证:   HMAC 密码验证\n")
	} else {
		fmt.Printf("  认证:   无\n")
	}
	fmt.Printf("\n  正在连接...\n")

	err := t.Connect(ctx, cfg.ServerAddr, *mtuFlag, func(tunCfg client.TunConfig) (client.TunDevice, error) {
		return tun.New(tun.Config{
			VirtualIP:  tunCfg.VirtualIP,
			SubnetMask: tunCfg.SubnetMask,
			ServerIP:   tunCfg.ServerIP,
			MTU:        tunCfg.MTU,
		})
	})
	if err != nil {
		return fmt.Errorf("连接失败: %w", err)
	}

	fmt.Println("  已断开。")
	return nil
}
