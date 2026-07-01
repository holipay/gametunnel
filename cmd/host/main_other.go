//go:build !windows

package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/paths"
	"github.com/holipay/gametunnel/internal/tun"
)

func main() {
	defer writeCrashLog()

	cfg, tunFactory, s := parseAndStart()
	run(cfg, tunFactory, s)
}

func writeCrashLog() {
	r := recover()
	if r == nil {
		return
	}

	logDir := paths.GameTunnelDir()
	os.MkdirAll(logDir, 0755)
	f, err := os.OpenFile(filepath.Join(logDir, "crash.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Panic: %v\n%s\n", r, debug.Stack())
		os.Exit(1)
	}
	defer f.Close()

	fmt.Fprintf(f, "=== Crash %s ===\n", "GameTunnel Host")
	fmt.Fprintf(f, "Panic: %v\n\n", r)
	fmt.Fprintf(f, "Stack:\n%s\n", debug.Stack())
	fmt.Fprintf(os.Stderr, "Panic: %v\nSee crash.log for details.\n", r)
	os.Exit(1)
}

func newTUNFactory(serverListenIP net.IP) func(client.TunConfig) (client.TunDevice, error) {
	return func(tunCfg client.TunConfig) (client.TunDevice, error) {
		return tun.New(tun.Config{
			VirtualIP:      tunCfg.VirtualIP,
			SubnetMask:     tunCfg.SubnetMask,
			ServerIP:       tunCfg.ServerIP,
			ServerPublicIP: serverListenIP,
			MTU:            tunCfg.MTU,
		})
	}
}
