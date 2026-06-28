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
type Device struct {
	tunDev          tun.Device
	name            string
	virtualIP       net.IP
	subnetMask      net.IPMask
	serverPublicIP  net.IP
	mtu             int
	physicalGateway string
	physicalIfIdx   int // physical NIC interface index for IPv6 route cleanup
}

func New(cfg Config) (*Device, error) {
	if cfg.MTU <= 0 {
		cfg.MTU = DefaultMTU
	}
	if cfg.VirtualIP.To4() == nil {
		return nil, fmt.Errorf("virtual IP must be an IPv4 address")
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

// ReadBatch reads up to batchSize packets from the TUN device in a single syscall.
// Returns the number of packets read and per-packet sizes.
func (d *Device) ReadBatch(bufs [][]byte, sizes []int) (int, error) {
	n, err := d.tunDev.Read(bufs, sizes, 0)
	return n, err
}

// WriteBatch writes multiple packets to the TUN device in a single syscall.
// Returns the number of packets written.
func (d *Device) WriteBatch(bufs [][]byte) (int, error) {
	n, err := d.tunDev.Write(bufs, 0)
	return n, err
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

func (d *Device) Close() error {
	d.CleanupRoutes()
	return d.tunDev.Close()
}

// Write writes a single packet to the TUN device.
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

func runCmdOutput(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %v: %w (%s)", name, args, err, string(out))
	}
	return string(out), nil
}
