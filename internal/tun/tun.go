//go:build windows

package tun

import (
	"fmt"
	"log"
	"net"
	"os/exec"
	"strings"

	"golang.zz2c4.com/wireguard/tun"
)

const (
	DefaultMTU = 1400
)

// Device represents an active TUN device with its virtual IP.
type Device struct {
	tunDev        tun.Device
	name          string
	virtualIP     net.IP
	subnetMask    net.IPMask
	mtu           int
	readSizes     [1]int
	readPackets   [1][]byte
	writePackets  [1][]byte
}

func New(cfg Config) (*Device, error) {
	if cfg.MTU <= 0 {
		cfg.MTU = DefaultMTU
	}
	if cfg.VirtualIP == nil {
		return nil, fmt.Errorf("virtual IP is required")
	}

	tunDev, err := tun.CreateTUN("GameTunnel", cfg.MTU)
	if err != nil {
		return nil, fmt.Errorf("create TUN: %w", err)
	}

	name, err := tunDev.Name()
	if err != nil {
		tunDev.Close()
		return nil, fmt.Errorf("get TUN name: %w", err)
	}

	dev := &Device{
		tunDev:     tunDev,
		name:       name,
		virtualIP:  cfg.VirtualIP.To4(),
		subnetMask: cfg.SubnetMask,
		mtu:        cfg.MTU,
	}

	if err := dev.configure(); err != nil {
		tunDev.Close()
		return nil, fmt.Errorf("configure TUN: %w", err)
	}

	return dev, nil
}

// configure assigns the IP address, sets up routing, and ensures broadcast
// packets (255.255.255.255) are routed through the TUN interface.
func (d *Device) configure() error {
	mask := net.IP(d.subnetMask).String()
	ip := d.virtualIP.String()

	// Step 1: Assign static IP
	if err := RunCmd("netsh", "interface", "ip", "set", "address",
		fmt.Sprintf("name=%s", d.name),
		"static", ip, mask); err != nil {
		return fmt.Errorf("assign IP: %w", err)
	}

	// Step 2: Disable auto-metric on TUN and force metric=1
	// This is critical — without disabling auto-metric, Windows overrides the value.
	if err := RunCmd("netsh", "interface", "ip", "set", "interface",
		fmt.Sprintf("name=%s", d.name),
		"metricstore=disabled"); err != nil {
		log.Printf("[tun] disable auto-metric warning: %v", err)
	}
	if err := RunCmd("netsh", "interface", "ip", "set", "interface",
		fmt.Sprintf("name=%s", d.name),
		"metric=1"); err != nil {
		log.Printf("[tun] set interface metric warning: %v", err)
	}

	// Step 3: Raise metrics on physical NICs so TUN wins broadcast routing
	raisePhysicalNICMetrics(d.name)

	// Step 4: Add subnet route
	subnet := d.virtualIP.Mask(d.subnetMask)
	maskBits, _ := d.subnetMask.Size()
	subnetStr := fmt.Sprintf("%s/%d", subnet, maskBits)
	if err := RunCmd("route", "add", subnetStr, "mask", mask, ip, "metric", "1"); err != nil {
		log.Printf("[tun] route add warning: %v", err)
	}

	// Step 5: Route global broadcast (255.255.255.255) through TUN
	// Games like StarCraft send UDP broadcasts to 255.255.255.255:6112 for LAN discovery.
	// On Windows, limited broadcast bypasses the routing table by default.
	// With metric=1 on TUN and higher metrics on physical NICs, the OS prefers TUN.
	if err := RunCmd("route", "add", "255.255.255.255", "mask", "255.255.255.255", ip, "metric", "1"); err != nil {
		log.Printf("[tun] broadcast route warning: %v", err)
	}

	// Step 6: Also add subnet broadcast route (e.g., 10.10.0.255)
	// Some games use directed broadcast instead of limited broadcast.
	subnetBroadcast := net.IP(make([]byte, 4))
	for i := 0; i < 4; i++ {
		subnetBroadcast[i] = subnet[i] | ^d.subnetMask[i]
	}
	if err := RunCmd("route", "add", subnetBroadcast.String(), "mask", mask, ip, "metric", "1"); err != nil {
		log.Printf("[tun] subnet broadcast route warning: %v", err)
	}

	return nil
}

// raisePhysicalNICMetrics enumerates active network interfaces and raises
// their interface metrics so the TUN (metric=1) is preferred for broadcast routing.
//
// On Windows, 255.255.255.255 limited broadcasts go out the interface with
// the lowest metric. By setting physical NICs to metric=100 and TUN to metric=1,
// all broadcast traffic enters the tunnel.
//
// We use PowerShell for reliable enumeration. Failures are non-fatal.
func raisePhysicalNICMetrics(tunName string) {
	// Get all UP, non-loopback, non-TUN interfaces
	ps := `Get-NetAdapter | Where-Object { $_.Status -eq 'Up' -and $_.Name -ne '%s' -and $_.InterfaceDescription -notmatch 'Loopback' } | Select-Object -ExpandProperty Name`
	ps = fmt.Sprintf(ps, tunName)

	out, err := runCmdOutput("powershell", "-NoProfile", "-Command", ps)
	if err != nil {
		log.Printf("[tun] enumerate NICs warning: %v", err)
		return
	}

	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		nicName := strings.TrimSpace(line)
		if nicName == "" || nicName == tunName {
			continue
		}
		// Raise metric to 100. The default gateway route on this NIC keeps
		// normal internet traffic working, but broadcast routing prefers TUN.
		if err := RunCmd("netsh", "interface", "ip", "set", "interface",
			fmt.Sprintf("name=%s", nicName),
			"metric=100"); err != nil {
			log.Printf("[tun] raise metric for %q warning: %v", nicName, err)
		} else {
			log.Printf("[tun] raised metric for NIC %q to 100", nicName)
		}
	}
}

// runCmdOutput executes a command and returns its combined stdout as a string.
func runCmdOutput(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %v: %w (%s)", name, args, err, string(out))
	}
	return string(out), nil
}
