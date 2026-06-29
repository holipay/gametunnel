//go:build !windows

package tun

import (
	"fmt"
	"net"
	"os/exec"

	"golang.zx2c4.com/wireguard/tun"
)

type Device struct {
	tunDev          tun.Device
	name            string
	virtualIP       net.IP
	subnetMask      net.IPMask
	serverPublicIP  net.IP
	mtu             int
	physicalGateway string
}

func New(cfg Config) (*Device, error) {
	if cfg.MTU <= 0 {
		cfg.MTU = DefaultMTU
	}
	if cfg.VirtualIP.To4() == nil {
		return nil, fmt.Errorf("virtual IP must be an IPv4 address")
	}

	tunDev, err := tun.CreateTUN("gt0", cfg.MTU)
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

// MTU returns the configured MTU of the TUN device.
func (d *Device) MTU() int { return d.mtu }

// ReadBatch reads up to batchSize packets from the TUN device in a single syscall.
func (d *Device) ReadBatch(bufs [][]byte, sizes []int) (int, error) {
	return d.tunDev.Read(bufs, sizes, 0)
}

// WriteBatch writes multiple packets to the TUN device in a single syscall.
func (d *Device) WriteBatch(bufs [][]byte) (int, error) {
	return d.tunDev.Write(bufs, 0)
}

// Read reads a single packet from the TUN device.
func (d *Device) Read(buf []byte) (int, error) {
	bufs := [1][]byte{buf}
	sizes := [1]int{}
	n, err := d.tunDev.Read(bufs[:], sizes[:], 0)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	return sizes[0], nil
}

func (d *Device) Write(data []byte) (int, error) {
	bufs := [1][]byte{data}
	n, err := d.tunDev.Write(bufs[:], 0)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	return len(data), nil
}

func (d *Device) Close() error {
	d.CleanupRoutes()
	return d.tunDev.Close()
}

func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %s", name, args, string(out))
	}
	return nil
}
