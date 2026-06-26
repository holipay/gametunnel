//go:build !windows

package main

func setupFirewallPlatform() (func(), error) {
	return func() {}, nil
}
