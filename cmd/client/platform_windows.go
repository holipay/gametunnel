//go:build windows

package main

import (
	"github.com/holipay/gametunnel/internal/tun"
)

// setupFirewallPlatform adds Windows Firewall rules. Returns cleanup func.
func setupFirewallPlatform() (func(), error) {
	cleanup, err := tun.SetupFirewall()
	if err != nil {
		return func() {}, err
	}
	return cleanup, nil
}
