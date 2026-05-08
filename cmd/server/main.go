// GameTunnel Server — 局域网游戏隧道
//
// Usage:
//
//	gtunnel-server -addr :4700 -subnet 10.10.0.0/24 -max 10
//	gtunnel-server -addr :4700 -password myroomsecret
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/holipay/gametunnel/internal/server"
	"github.com/holipay/gametunnel/internal/singleinstance"
)

// Version is set at build time via -ldflags.
var Version = "dev"

func main() {
	addr := flag.String("addr", ":4700", "监听地址 (UDP)")
	subnetStr := flag.String("subnet", "10.10.0.0/24", "虚拟子网 (CIDR)")
	maxPlayers := flag.Int("max", 10, "最大玩家数")
	roomPass := flag.String("password", "", "房间密码（留空=无认证）")
	statusAddr := flag.String("status-addr", "", "状态页面地址 (HTTP)，如 :4701")
	versionFlag := flag.Bool("version", false, "显示版本")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("gtunnel-server %s\n", Version)
		os.Exit(0)
	}

	// ====== Single-instance check ======
	lock, err := singleinstance.Acquire("GameTunnel-Server")
	if err != nil {
		log.Fatalf("检测到已有实例运行中: %v\n请先关闭已有的 GameTunnel 服务端再重试。", err)
	}
	defer lock.Close()

	_, subnet, err := net.ParseCIDR(*subnetStr)
	if err != nil {
		log.Fatalf("子网解析失败 %x: %v", *subnetStr, err)
	}

	s, err := server.New(server.Config{
		Addr:        *addr,
		Subnet:      subnet,
		MaxPlayers:  *maxPlayers,
		RoomPass:    *roomPass,
		StatusAddr:  *statusAddr,
		Version:     Version,
	})
	if err != nil {
		log.Fatalf("启动失败: %v", err)
	}

	// Graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		log.Printf("收到信号 %v，正在关闭...", sig)
		cancel()
		s.Close()
	}()

	// Print banner
	authStatus := "无认证"
	if *roomPass != "" {
		authStatus = "HMAC 认证 (基于房间ID)"
	}
	log.Printf("════════════════════════════════════════════════════════════")
	log.Printf("▎         GameTunnel Server 局域网游戏隧道                ▎")
	log.Printf("════════════════════════════════════════════════════════════")
	log.Printf("▎  地址：   %-31s ▎", *addr)
	log.Printf("▎  子网：   %-31s ▎", subnet.String())
	log.Printf("▎  最大玩家：  %-31d ▎", *maxPlayers)
	log.Printf("▎  认证：   %-31s ▎", authStatus)
	log.Printf("▎  版本：   %-31s ▎", Version)
	if *statusAddr != "" {
		log.Printf("▎  状态页：   %-31s ▎", fmt.Sprintf("http://%s", *statusAddr))
	}
	log.Printf("════════════════════════════════════════════════════════════")

	s.Run(ctx)
	log.Println("服务器已关闭")
}
