//go:build windows

package tun

import (
	"log"
	"os"
)

const (
	firewallRuleNameApp  = "GameTunnel Client"
	firewallRuleNamePort = "GameTunnel UDP Tunnel"
)

// SetupFirewall adds Windows Firewall rules to allow GameTunnel traffic.
// It allows the executable through inbound, and opens the UDP tunnel port.
// Requires admin privileges (already elevated by requestAdmin()).
// Returns a cleanup function that removes the rules on shutdown.
func SetupFirewall() (cleanup func(), err error) {
	exePath, err := os.Executable()
	if err != nil {
		return func() {}, nil // non-fatal, skip
	}

	// Rule 1: Allow gtunnel-client.exe inbound (game traffic from TUN)
	// This covers both the tunnel UDP socket and any game traffic the TUN captures.
	RunCmd("netsh", "advfirewall", "firewall", "add", "rule",
		"name="+firewallRuleNameApp,
		"dir=in",
		"action=allow",
		"program="+exePath,
		"enable=yes",
		"profile=any",
		"protocol=any",
	)

	// Rule 2: Allow gtunnel-client.exe outbound (tunnel to server)
	RunCmd("netsh", "advfirewall", "firewall", "add", "rule",
		"name="+firewallRuleNameApp+" Out",
		"dir=out",
		"action=allow",
		"program="+exePath,
		"enable=yes",
		"profile=any",
		"protocol=any",
	)

	log.Printf("[tun] firewall rules added for %s", exePath)

	// Return cleanup function
	cleanup = func() {
		RunCmd("netsh", "advfirewall", "firewall", "delete", "rule",
			"name="+firewallRuleNameApp,
		)
		RunCmd("netsh", "advfirewall", "firewall", "delete", "rule",
			"name="+firewallRuleNameApp+" Out",
		)
		log.Printf("[tun] firewall rules removed")
	}

	return cleanup, nil
}
