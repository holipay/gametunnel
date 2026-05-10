//go:build linux

package tun

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/unix"
)

const tunDevice = "/dev/net/tun"

// Device represents an active TUN device (Linux).
type Device struct {
	file          *os.File
	name          string
	virtualIP     net.IP
	subnetMask    net.IPMask
	serverPublicIP net.IP
	mtu           int
}

func New(cfg Config) (*Device, error) {
	if cfg.MTU <= 0 {
		cfg.MTU = DefaultMTU
	}
	if cfg.VirtualIP == nil {
		return nil, fmt.Errorf("virtual IP is required")
	}

	// Open /dev/net/tun
	fd, err := os.OpenFile(tunDevice, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w (need root or CAP_NET_ADMIN)", tunDevice, err)
	}

	// Create TUN interface via ioctl
	var ifr struct {
		Name  [16]byte
		Flags uint16
		_     [22]byte // padding
	}
	copy(ifr.Name[:], "gtun\x00")
	ifr.Flags = unix.IFF_TUN | unix.IFF_NO_PI // TUN (not TAP), no packet info header

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, fd.Fd(), unix.TUNSETIFF, uintptr(unsafe.Pointer(&ifr)))
	if errno != 0 {
		fd.Close()
		return nil, fmt.Errorf("TUNSETIFF: %v", errno)
	}

	name := unix.ByteToString(ifr.Name[:])

	dev := &Device{
		file:           fd,
		name:           name,
		virtualIP:      cfg.VirtualIP.To4(),
		subnetMask:     cfg.SubnetMask,
		serverPublicIP: cfg.ServerPublicIP,
		mtu:            cfg.MTU,
	}

	if err := dev.configure(); err != nil {
		fd.Close()
		return nil, fmt.Errorf("configure TUN: %w", err)
	}

	return dev, nil
}

func (d *Device) Name() string { return d.name }

func (d *Device) Read(buf []byte) (int, error) {
	return d.file.Read(buf)
}

func (d *Device) Write(data []byte) (int, error) {
	return d.file.Write(data)
}

func (d *Device) Close() error {
	d.cleanupRoutes()
	return d.file.Close()
}

// configure assigns IP, brings interface up, and sets routes.
func (d *Device) configure() error {
	ip := d.virtualIP.String()
	mask := net.IP(d.subnetMask).String()

	// Assign IP
	if err := runCmd("ip", "addr", "add", ip+"/32", "dev", d.name); err != nil {
		return fmt.Errorf("assign IP: %w", err)
	}

	// Bring interface up
	if err := runCmd("ip", "link", "set", d.name, "up"); err != nil {
		return fmt.Errorf("link up: %w", err)
	}

	// Set MTU
	if err := runCmd("ip", "link", "set", d.name, "mtu", fmt.Sprintf("%d", d.mtu)); err != nil {
		return fmt.Errorf("set MTU: %w", err)
	}

	// Add route for the tunnel subnet
	ones, _ := d.subnetMask.Size()
	subnet := fmt.Sprintf("%s/%d", d.virtualIP.Mask(d.subnetMask).String(), ones)
	if err := runCmd("ip", "route", "add", subnet, "dev", d.name); err != nil {
		return fmt.Errorf("add route: %w", err)
	}

	_ = mask // used by Windows, kept for interface compatibility
	return nil
}

// cleanupRoutes removes routes added by configure.
func (d *Device) cleanupRoutes() {
	ones, _ := d.subnetMask.Size()
	subnet := fmt.Sprintf("%s/%d", d.virtualIP.Mask(d.subnetMask).String(), ones)
	runCmd("ip", "route", "del", subnet, "dev", d.name)
}

func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %s", name, args, string(out))
	}
	return nil
}
