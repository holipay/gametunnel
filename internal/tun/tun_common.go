package tun

import "net"

const DefaultMTU = 1400

// Config holds TUN device configuration.
type Config struct {
	VirtualIP      net.IP
	SubnetMask     net.IPMask
	ServerIP       net.IP // server's virtual IP on tunnel subnet
	ServerPublicIP net.IP // server's public IP (for route exclusion)
	MTU            int
}
