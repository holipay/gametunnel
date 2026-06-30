package tun

import (
	"fmt"
	"net"
)

const DefaultMTU = 1400

// Config holds TUN device configuration.
type Config struct {
	VirtualIP      net.IP
	SubnetMask     net.IPMask
	ServerIP       net.IP // server's virtual IP on tunnel subnet
	ServerPublicIP net.IP // server's public IP (for route exclusion)
	MTU            int
}

// validateConfig checks that config values are safe to use in OS commands.
// Returns an error if any value contains unexpected characters.
func validateConfig(cfg *Config) error {
	if cfg.VirtualIP != nil && net.ParseIP(cfg.VirtualIP.String()) == nil {
		return fmt.Errorf("invalid VirtualIP: %s", cfg.VirtualIP)
	}
	if cfg.ServerIP != nil && net.ParseIP(cfg.ServerIP.String()) == nil {
		return fmt.Errorf("invalid ServerIP: %s", cfg.ServerIP)
	}
	if cfg.ServerPublicIP != nil && net.ParseIP(cfg.ServerPublicIP.String()) == nil {
		return fmt.Errorf("invalid ServerPublicIP: %s", cfg.ServerPublicIP)
	}
	return nil
}
