// GameTunnel Client — 通用局域网游戏隧道 (Windows)
//
// CLI 模式：直接在终端运行，Ctrl+C 断开。
//
// Usage:
//
//	gtunnel-client.exe -server 1.2.3.4:4700
//	gtunnel-client.exe -server 1.2.3.4:4700 -name Player1 -room myroom -password secret
//	gtunnel-client.exe  (使用配置文件)
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/tun"
)

// Version is set at build time via -ldflags.
var Version = "dev"

func main() {
	serverFlag := flag.String("server", "", "服务器地址 (host:port)")
	nameFlag := flag.String("name", "", "玩家名称")
	roomFlag := flag.String("room", "", "房间ID")
	passFlag := flag.String("password", "", "房间密码")
	mtuFlag := flag.Int("mtu", 1400, "隧道 MTU")
	versionFlag := flag.Bool("version", false, "显示版本")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("gtunnel-client %s\n", Version)
		os.Exit(0)
	}

	// Load config (CLI flags override)
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
	if cfg.ServerAddr == "" {
		path := client.CreateDefaultConfig()
		fmt.Fprintf(os.Stderr, "\n  首次运行，已创建配置文件:\n")
		fmt.Fprintf(os.Stderr, "  %s\n\n", path)
		fmt.Fprintf(os.Stderr, "  请用记事本编辑此文件，填入服务器地址后重新运行。\n")
		fmt.Fprintf(os.Stderr, "  或使用命令行: gtunnel-client.exe -server 你的服务器IP:4700\n\n")
		os.Exit(1)
	}

	// Save config so subsequent runs remember CLI overrides
	client.SaveConfig(cfg)

	// Setup logging (file + stderr)
	logFile := client.SetupLog()
	defer logFile.Close()

	// Setup signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	t := client.New(cfg)

	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\n正在断开...")
		cancel()
		t.Disconnect()
	}()

	fmt.Printf("🎮 GameTunnel 客户端 %s\n", Version)
	fmt.Printf("   服务器: %s\n", cfg.ServerAddr)
	fmt.Printf("   玩家:   %s\n", cfg.PlayerName)
	fmt.Printf("   房间:   %s\n", cfg.RoomID)
	if cfg.RoomPassword != "" {
		fmt.Printf("   认证:   HMAC 密码验证\n")
	} else {
		fmt.Printf("   认证:   无\n")
	}
	fmt.Printf("\n正在连接...\n")

	err := t.Connect(ctx, cfg.ServerAddr, *mtuFlag, func(tunCfg client.TunConfig) (client.TunDevice, error) {
		return tun.New(tun.Config{
			VirtualIP:  tunCfg.VirtualIP,
			SubnetMask: tunCfg.SubnetMask,
			ServerIP:   tunCfg.ServerIP,
			MTU:        tunCfg.MTU,
		})
	})
	if err != nil {
		log.Fatalf("连接失败: %v", err)
	}

	fmt.Println("已断开。")
}
