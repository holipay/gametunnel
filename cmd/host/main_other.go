//go:build !windows

package main

import (
	"net"

	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/crashlog"
	"github.com/holipay/gametunnel/internal/tun"
)

func main() {
	defer crashlog.WriteCrashLog("GameTunnel Host")

	cfg, tunFactory, s := parseAndStart()
	run(cfg, tunFactory, s)
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
