//go:build windows

package tun

import (
	"log"
	"os"
)

const (
	firewallRuleNameApp     = "GameTunnel Client"
	firewallRuleNameAppOut  = "GameTunnel Client Out"
	firewallRuleNameGameLAN = "GameTunnel LAN Ports"
)

// commonLANPorts lists UDP/TCP ports used by popular LAN games.
// These are opened inbound so the game can receive traffic through the TUN.
//
//	Port   Protocol  Games
//	6112   UDP+TCP   StarCraft, StarCraft: Brood War, Diablo II, Warcraft II
//	6113   UDP       StarCraft (alternate)
//	47624  UDP       Some older games
var commonLANPorts = []struct {
	Port     string
	Protocol string
}{
	{"6112", "udp"},
	{"6112", "tcp"},
	{"6113", "udp"},
	{"47624", "udp"},
}

// SetupFirewall adds Windows Firewall rules to allow GameTunnel traffic.
//
// It creates three groups of rules:
//  1. Allow gtunnel-client.exe all traffic (tunnel UDP + TUN relay)
//  2. Open common LAN game ports inbound for ANY program (game receives TUN packets)
//
// Requires admin privileges (already elevated by requestAdmin()).
// Returns a cleanup function that removes all rules on shutdown.
func SetupFirewall() (cleanup func(), err error) {
	exePath, err := os.Executable()
	if err != nil {
		return func() {}, nil // non-fatal, skip
	}

	// ── Rule group 1: gtunnel-client.exe ──

	// Inbound: tunnel receives game packets from TUN + UDP from server
	RunCmd("netsh", "advfirewall", "firewall", "add", "rule",
		"name="+firewallRuleNameApp,
		"dir=in",
		"action=allow",
		"program="+exePath,
		"enable=yes",
		"profile=any",
		"protocol=any",
	)

	// Outbound: tunnel sends to server + writes to TUN
	RunCmd("netsh", "advfirewall", "firewall", "add", "rule",
		"name="+firewallRuleNameAppOut,
		"dir=out",
		"action=allow",
		"program="+exePath,
		"enable=yes",
		"profile=any",
		"protocol=any",
	)

	// ── Rule group 2: LAN game ports ──
	// The game (e.g., starcraft.exe) is a separate process that receives
	// packets written to the TUN by gtunnel-client. Without these rules,
	// Windows Firewall blocks the game from receiving TUN traffic.
	for _, p := range commonLANPorts {
		RunCmd("netsh", "advfirewall", "firewall", "add", "rule",
			"name="+firewallRuleNameGameLAN,
			"dir=in",
			"action=allow",
			"program=any",
			"enable=yes",
			"profile=any",
			"protocol="+p.Protocol,
			"localport="+p.Port,
		)
	}

	log.Printf("[tun] firewall rules added (app: %s, LAN ports: 6112/udp, 6112/tcp, 6113/udp, 47624/udp)", exePath)

	// Return cleanup function
	cleanup = func() {
		RunCmd("netsh", "advfirewall", "firewall", "delete", "rule",
			"name="+firewallRuleNameApp,
		)
		RunCmd("netsh", "advfirewall", "firewall", "delete", "rule",
			"name="+firewallRuleNameAppOut,
		)
		RunCmd("netsh", "advfirewall", "firewall", "delete", "rule",
			"name="+firewallRuleNameGameLAN,
		)
		log.Printf("[tun] firewall rules removed")
	}

	return cleanup, nil
}
