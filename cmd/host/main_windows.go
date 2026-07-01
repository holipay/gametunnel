//go:build windows

package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"

	"golang.org/x/sys/windows"

	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/paths"
	"github.com/holipay/gametunnel/internal/tun"
)

func main() {
	defer writeCrashLog()

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

func writeCrashLog() {
	r := recover()
	if r == nil {
		return
	}

	logDir := paths.GameTunnelDir()
	os.MkdirAll(logDir, 0755)
	f, err := os.OpenFile(filepath.Join(logDir, "crash.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return
	}
	defer f.Close()

	fmt.Fprintf(f, "=== Crash %s ===\n", "GameTunnel Host")
	fmt.Fprintf(f, "Panic: %v\n\n", r)
	fmt.Fprintf(f, "Stack:\n%s\n", debug.Stack())
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
