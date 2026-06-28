//go:build !windows

package main

import (
	"fmt"
	"log"
	"os"
	"runtime/debug"

	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/singleinstance"
	"github.com/holipay/gametunnel/internal/tun"
)

func main() {
	defer writeCrashLog()

	// Prevent multiple instances
	if _, err := singleinstance.Acquire("GameTunnel-Client"); err != nil {
		log.Printf("single instance check: %v", err)
		fmt.Println("GameTunnel is already running.")
		os.Exit(0)
	}

	cfg := client.LoadConfig()

	if cfg.Lang != "" {
		i18n.Set(i18n.ParseLang(cfg.Lang))
	}

	serverPublicIP := parseHostIP(cfg.ServerAddr)

	tunFactory := func(tunCfg client.TunConfig) (client.TunDevice, error) {
		return tun.New(tun.Config{
			VirtualIP:      tunCfg.VirtualIP,
			SubnetMask:     tunCfg.SubnetMask,
			ServerIP:       tunCfg.ServerIP,
			ServerPublicIP: serverPublicIP,
			MTU:            tunCfg.MTU,
		})
	}

	run(cfg, tunFactory)
}

func writeCrashLog() {
	r := recover()
	if r == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "Panic: %v\n%s\n", r, debug.Stack())
	os.Exit(1)
}
