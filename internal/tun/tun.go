// Package tun handles creation and management of TUN (virtual network) devices.
//
// On Linux: creates /dev/net/tun device
// On macOS: creates utun device
// Both present as a virtual network interface that captures IP packets.
package tun

import (
	"fmt"
	"net"
	"os"
	"runtime"

	"golang.zx2c4.com/wireguard/tun"
)

const (
	// MTU for the tunnel. 1400 is safe for most game traffic through UDP.
	DefaultMTU = 1400
)

// Device represents an active TUN device with its virtual IP.
type Device struct {
	tunDev   tun.Device
	name     string
	virtualIP  net.IP
	subnetMask net.IPMask
	mtu        int
	file       *os.File // for direct read/write fallback
}

// Config holds parameters for creating a TUN device.
type Config struct {
	VirtualIP  net.IP
	SubnetMask net.IPMask
	ServerIP   net.IP // needed for route setup
	MTU        int
}

// New creates and configures a TUN device.
// It assigns the given virtual IP and sets up routing to the tunnel subnet.
func New(cfg Config) (*Device, error) {
	if cfg.MTU <= 0 {
		cfg.MTU = DefaultMTU
	}
	if cfg.VirtualIP == nil {
		return nil, fmt.Errorf("virtual IP is required")
	}

	// Create TUN device via wireguard-go
	tunDev, name, err := tun.CreateTUN("gtun", cfg.MTU)
	if err != nil {
		return nil, fmt.Errorf("create TUN: %w", err)
	}

	dev := &Device{
		tunDev:     tunDev,
		name:       name,
		virtualIP:  cfg.VirtualIP.To4(),
		subnetMask: cfg.SubnetMask,
		mtu:        cfg.MTU,
	}

	// Configure the interface: assign IP and bring it up
	if err := dev.configure(cfg.ServerIP); err != nil {
		tunDev.Close()
		return nil, fmt.Errorf("configure TUN: %w", err)
	}

	return dev, nil
}

// configure assigns the IP address and brings the interface up.
func (d *Device) configure(serverIP net.IP) error {
	switch runtime.GOOS {
	case "linux":
		return d.configureLinux(serverIP)
	case "darwin":
		return d.configureDarwin(serverIP)
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

func (d *Device) configureLinux(serverIP net.IP) error {
	maskBits, _ := d.subnetMask.Size()
	// Assign IP: ip addr add 10.10.0.2/24 dev gtun
	if err := runCmd("ip", "addr", "add",
		fmt.Sprintf("%s/%d", d.virtualIP, maskBits),
		"dev", d.name); err != nil {
		return fmt.Errorf("assign IP: %w", err)
	}
	// Bring up: ip link set gtun up
	if err := runCmd("ip", "link", "set", d.name, "up"); err != nil {
		return fmt.Errorf("bring up: %w", err)
	}
	// Route to subnet: ip route add 10.10.0.0/24 dev gtun
	subnet := d.virtualIP.Mask(d.subnetMask)
	if err := runCmd("ip", "route", "add",
		fmt.Sprintf("%s/%d", subnet, maskBits),
		"dev", d.name); err != nil {
		// Route may already exist, not fatal
		fmt.Printf("[tun] route add warning: %v (may already exist)\n", err)
	}
	return nil
}

func (d *Device) configureDarwin(serverIP net.IP) error {
	mask := net.IP(d.subnetMask).String()
	// macOS: ifconfig utunX <vip> <vip> netmask <mask> up
	if err := runCmd("ifconfig", d.name,
		d.virtualIP.String(), d.virtualIP.String(),
		"netmask", mask, "up"); err != nil {
		return fmt.Errorf("ifconfig: %w", err)
	}
	// Route to subnet
	subnet := d.virtualIP.Mask(d.subnetMask)
	maskBits, _ := d.subnetMask.Size()
	if err := runCmd("route", "-n", "add",
		"-net", fmt.Sprintf("%s/%d", subnet, maskBits),
		"-interface", d.name); err != nil {
		fmt.Printf("[tun] route add warning: %v\n", err)
	}
	return nil
}

// Read reads an IP packet from the TUN device.
// Returns the raw IP packet (without any framing).
func (d *Device) Read(buf []byte) (int, error) {
	sizes := make([]int, 1)
	packets := make([][]byte, 1)
	packets[0] = buf
	_, err := d.tunDev.Read(packets, sizes, 0)
	if err != nil {
		return 0, err
	}
	if sizes[0] == 0 {
		return 0, fmt.Errorf("zero-length read from TUN")
	}
	return sizes[0], nil
}

// Write writes an IP packet to the TUN device.
func (d *Device) Write(data []byte) (int, error) {
	sizes := []int{len(data)}
	packets := [][]byte{data}
	_, err := d.tunDev.Write(packets, 0)
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

// Close shuts down the TUN device.
func (d *Device) Close() error {
	return d.tunDev.Close()
}

// Name returns the interface name (e.g., "gtun0").
func (d *Device) Name() string {
	return d.name
}

// VirtualIP returns the assigned virtual IP.
func (d *Device) VirtualIP() net.IP {
	return d.virtualIP
}

// runCmd executes a system command and returns an error if it fails.
func runCmd(name string, args ...string) error {
	// We import os/exec in the platform-specific files
	return platformRunCmd(name, args...)
}
