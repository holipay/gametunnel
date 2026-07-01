//go:build !windows

package main

import (
	"fmt"
	"net"
	"os"
	"runtime/debug"

	"github.com/holipay/gametunnel/internal/client"
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
	fmt.Fprintf(os.Stderr, "Panic: %v\n%s\n", r, debug.Stack())
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
