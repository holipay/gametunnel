//go:build windows

package main

import (
	"fmt"
	"log"
	"net"
	"os"

	"golang.org/x/sys/windows"

	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/crashlog"
	"github.com/holipay/gametunnel/internal/tun"
)

func main() {
	defer crashlog.WriteCrashLog("GameTunnel Host")

	windows.SetConsoleOutputCP(65001)

	// Request admin rights if not elevated (needed for TUN device).
	requestAdmin()

	cfg, tunFactory, s := parseAndStart()
	run(cfg, tunFactory, s)
}

func requestAdmin() {
	token := windows.GetCurrentProcessToken()
	if token.IsElevated() {
		return
	}

	exe, err := os.Executable()
	if err != nil {
		return
	}

	verb, _ := windows.UTF16PtrFromString("runas")
	exePath, _ := windows.UTF16PtrFromString(exe)

	if err := windows.ShellExecute(0, verb, exePath, nil, nil, windows.SW_SHOWNORMAL); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to elevate: %v\n", err)
		os.Exit(1)
	}

	os.Exit(0)
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
