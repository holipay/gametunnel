//go:build windows

package tun

import (
	"fmt"
	"net"
	"os/exec"
	"syscall"

	"golang.zx2c4.com/wireguard/tun"
)

// Device represents an active TUN device with its virtual IP (Windows).
//
// TODO: batch=32 for high-throughput scenarios (>100 pps). Current batch=1 is
// fine for games but wastes the wireguard/tun batch interface for bulk transfer.
type Device struct {
	tunDev          tun.Device
	name            string
	virtualIP       net.IP
	subnetMask      net.IPMask
	serverPublicIP  net.IP
	mtu             int
	readSizes       [1]int
	readPackets     [1][]byte
	writePackets    [1][]byte
	physicalGateway string
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
		tunDev:         tunDev,
		name:           name,
		virtualIP:      cfg.VirtualIP.To4(),
		subnetMask:     cfg.SubnetMask,
		serverPublicIP: cfg.ServerPublicIP,
		mtu:            cfg.MTU,
	}

	if err := dev.configure(); err != nil {
		tunDev.Close()
		return nil, fmt.Errorf("configure TUN: %w", err)
	}

	return dev, nil
}

func (d *Device) Name() string { return d.name }

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

func (d *Device) Close() error {
	d.CleanupRoutes()
	return d.tunDev.Close()
}

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

func runCmdOutput(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %v: %w (%s)", name, args, err, string(out))
	}
	return string(out), nil
}
