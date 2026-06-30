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
	offset          int // virtioNetHdrLen when vnetHdr is enabled, else 0
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

	if tunDev.BatchSize() > 1 {
		dev.offset = 10 // virtioNetHdr size
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
	n, err := d.tunDev.Read(bufs, sizes, d.offset)
	if err != nil {
		return 0, err
	}
	if d.offset > 0 {
		for i := 0; i < n; i++ {
			copy(bufs[i], bufs[i][d.offset:d.offset+sizes[i]])
		}
	}
	return n, nil
}

// WriteBatch writes multiple packets to the TUN device in a single syscall.
func (d *Device) WriteBatch(bufs [][]byte) (int, error) {
	if d.offset > 0 {
		padded := make([][]byte, len(bufs))
		for i, b := range bufs {
			p := make([]byte, d.offset+len(b))
			copy(p[d.offset:], b)
			padded[i] = p
		}
		return d.tunDev.Write(padded, d.offset)
	}
	return d.tunDev.Write(bufs, 0)
}

// Read reads a single packet from the TUN device.
func (d *Device) Read(buf []byte) (int, error) {
	bufs := [1][]byte{buf}
	sizes := [1]int{}
	n, err := d.tunDev.Read(bufs[:], sizes[:], d.offset)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	sz := sizes[0]
	if d.offset > 0 {
		// handleVirtioRead writes data to buf[offset:]; shift to buf[0:]
		// so the caller sees a standard IP packet starting at offset 0.
		copy(buf, buf[d.offset:d.offset+sz])
	}
	return sz, nil
}

func (d *Device) Write(data []byte) (int, error) {
	if d.offset > 0 {
		buf := make([]byte, d.offset+len(data))
		copy(buf[d.offset:], data)
		bufs := [1][]byte{buf}
		_, err := d.tunDev.Write(bufs[:], d.offset)
		if err != nil {
			return 0, err
		}
		return len(data), nil
	}
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
