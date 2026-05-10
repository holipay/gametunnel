//go:build !windows

package main

import (
	"fmt"
	"log"
	"net"
	"os"

	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/tun"
)

func main() {
	// Load config
	cfg := client.LoadConfig()

	// Parse server public IP for route exclusion
	serverPublicIP := parseHostIP(cfg.ServerAddr)

	// Setup TUN factory for Linux/macOS
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

// parseHostIP extracts the IP from a "host:port" address string.
func parseHostIP(addr string) net.IP {
	if addr == "" {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return net.ParseIP(addr)
	}
	return net.ParseIP(host)
}
