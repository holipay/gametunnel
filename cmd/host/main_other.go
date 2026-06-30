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

func init() {
	// Catch panics and write crash log
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "Panic: %v\n%s\n", r, debug.Stack())
			os.Exit(1)
		}
	}()
}

func newTUNFactory(serverPublicIP net.IP) func(client.TunConfig) (client.TunDevice, error) {
	return func(tunCfg client.TunConfig) (client.TunDevice, error) {
		return tun.New(tun.Config{
			VirtualIP:      tunCfg.VirtualIP,
			SubnetMask:     tunCfg.SubnetMask,
			ServerIP:       tunCfg.ServerIP,
			ServerPublicIP: serverPublicIP,
			MTU:            tunCfg.MTU,
		})
	}
}
