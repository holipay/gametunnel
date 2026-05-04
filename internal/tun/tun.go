//go:build windows

// Package tun handles creation and management of TUN (virtual network) devices
// on Windows via the WireGuard wintun driver.
package tun

import (
	"fmt"
	"log"
	"net"
	"os/exec"

	"golang.zx2c4.com/wireguard/tun"
)

const (
	DefaultMTU = 1400
)

// Device represents an active TUN device with its virtual IP.
type Device struct {
	tunDev     tun.Device
	name       string
	virtualIP  net.IP
	subnetMask net.IPMask
}

// Config holds parameters for creating a TUN device.
type Config struct {
	VirtualIP  net.IP
	SubnetMask net.IPMask
	ServerIP   net.IP
	MTU        int
}

// New creates and configures a TUN device on Windows.
func New(cfg Config) (*Device, error) {
	if cfg.MTU <= 0 {
		cfg.MTU = DefaultMTU
	}
	if cfg.VirtualIP == nil {
		return nil, fmt.Errorf("virtual IP is required")
	}

	tunDev, name, err := tun.CreateTUN("GameTunnel", cfg.MTU)
	if err != nil {
		return nil, fmt.Errorf("create TUN: %w", err)
	}

	dev := &Device{
		tunDev:     tunDev,
		name:       name,
		virtualIP:  cfg.VirtualIP.To4(),
		subnetMask: cfg.SubnetMask,
	}

	if err := dev.configure(); err != nil {
		tunDev.Close()
		return nil, fmt.Errorf("configure TUN: %w", err)
	}

	return dev, nil
}

// configure assigns the IP address and brings the interface up via netsh.
func (d *Device) configure() error {
	mask := net.IP(d.subnetMask).String()
	ip := d.virtualIP.String()

	// netsh interface ip set address "GameTunnel" static 10.10.0.2 255.255.255.0
	if err := runCmd("netsh", "interface", "ip", "set", "address",
		fmt.Sprintf("name=%s", d.name),
		"static", ip, mask); err != nil {
		return fmt.Errorf("assign IP: %w", err)
	}

	// Add subnet route (usually auto-added, but be explicit)
	subnet := d.virtualIP.Mask(d.subnetMask)
	maskBits, _ := d.subnetMask.Size()
	subnetStr := fmt.Sprintf("%s/%d", subnet, maskBits)

	// route add 10.10.0.0 mask 255.255.255.0 10.10.0.2
	if err := runCmd("route", "add", subnetStr, "mask", mask, ip, "metric", "1"); err != nil {
		// Not fatal — route may already exist
		log.Printf("[tun] route add warning: %v", err)
	}

	return nil
}

// Read reads an IP packet from the TUN device.
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

// Name returns the interface name.
func (d *Device) Name() string {
	return d.name
}

// VirtualIP returns the assigned virtual IP.
func (d *Device) VirtualIP() net.IP {
	return d.virtualIP
}

// runCmd executes a system command.
func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %s", name, args, string(out))
	}
	return nil
}
