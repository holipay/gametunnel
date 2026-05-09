//go:build windows

package tun

import (
	"fmt"
	"net"
	"os/exec"

	"golang.zx2c4.com/wireguard/tun"
)

const (
	DefaultMTU = 1400
)

// Config holds TUN device configuration.
type Config struct {
	VirtualIP  net.IP
	SubnetMask net.IPMask
	ServerIP   net.IP
	MTU        int
}

// Device represents an active TUN device with its virtual IP.
type Device struct {
	tunDev       tun.Device
	name         string
	virtualIP    net.IP
	subnetMask   net.IPMask
	mtu          int
	readSizes    [1]int
	readPackets  [1][]byte
	writePackets [1][]byte
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

// Name returns the TUN device name (e.g. "GameTunnel").
func (d *Device) Name() string {
	return d.name
}

// Read reads a packet from the TUN device. Satisfies client.TunDevice.
func (d *Device) Read(buf []byte) (int, error) {
	d.readPackets[0] = buf
	n, err := d.tunDev.Read(d.readPackets[:], d.readSizes[:], 0)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	return d.readSizes[0], nil
}

// Close closes the TUN device and releases resources. Satisfies client.TunDevice.
func (d *Device) Close() error {
	return d.tunDev.Close()
}

// Write writes a packet to the TUN device. Satisfies client.TunDevice.
func (d *Device) Write(data []byte) (int, error) {
	d.writePackets[0] = data
	n, err := d.tunDev.Write(d.writePackets[:], 0)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	return len(data), nil
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
