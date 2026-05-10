//go:build darwin

package tun

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/unix"
)

const SYSPROTO_CONTROL = 2

// Device represents an active TUN device (macOS utun).
type Device struct {
	fd            int
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

	// Create a utun socket pair
	fd, err := unix.Socket(unix.AF_SYSTEM, unix.SOCK_DGRAM, SYSPROTO_CONTROL)
	if err != nil {
		return nil, fmt.Errorf("create utun socket: %w", err)
	}

	// Connect to the utun control unit (device index 0 = let kernel choose)
	var addr struct {
		Len     uint8
		Family  uint8
		_       uint16 // ss_sysaddr
		ID      uint32
		Name    [96]byte
	}
	addr.Len = uint8(unsafe.Sizeof(addr))
	addr.Family = unix.AF_SYSTEM
	// SYSPROTO_CONTROL = 2
	addr.ID = 0 // utun device index, 0 = auto

	_, _, errno := unix.Syscall(unix.SYS_CONNECT, uintptr(fd),
		uintptr(unsafe.Pointer(&addr)), uintptr(addr.Len))
	if errno != 0 {
		unix.Close(fd)
		return nil, fmt.Errorf("connect utun: %v", errno)
	}

	// Get the utun interface name
	var ifName [16]byte
	nameLen := uint32(len(ifName))
	_, _, errno = unix.Syscall6(unix.SYS_GETSOCKOPT, uintptr(fd),
		SYSPROTO_CONTROL, 2, // UTUN_OPT_IFNAME
		uintptr(unsafe.Pointer(&ifName[0])), uintptr(unsafe.Pointer(&nameLen)), 0)
	if errno != 0 {
		unix.Close(fd)
		return nil, fmt.Errorf("get utun name: %v", errno)
	}

	n := bytes.IndexByte(ifName[:], 0)
	if n < 0 {
		n = len(ifName)
	}
	name := string(ifName[:n])

	dev := &Device{
		fd:            fd,
		name:          name,
		virtualIP:     cfg.VirtualIP.To4(),
		subnetMask:    cfg.SubnetMask,
		serverPublicIP: cfg.ServerPublicIP,
		mtu:           cfg.MTU,
	}

	if err := dev.configure(); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("configure TUN: %w", err)
	}

	return dev, nil
}

func (d *Device) Name() string { return d.name }

func (d *Device) Read(buf []byte) (int, error) {
	n, err := unix.Read(d.fd, buf)
	if err != nil {
		return 0, os.NewSyscallError("read", err)
	}
	// macOS utun prepends a 4-byte protocol family header (AF_INET=2).
	// Skip it and return the IP packet.
	if n > 4 {
		copy(buf, buf[4:n])
		return n - 4, nil
	}
	return 0, nil
}

func (d *Device) Write(data []byte) (int, error) {
	// Prepend 4-byte protocol family header (AF_INET = 2 for IPv4)
	pkt := make([]byte, 4+len(data))
	pkt[0] = 0
	pkt[1] = 0
	pkt[2] = 0
	pkt[3] = 2 // AF_INET
	copy(pkt[4:], data)
	n, err := unix.Write(d.fd, pkt)
	if err != nil {
		return 0, os.NewSyscallError("write", err)
	}
	if n >= 4 {
		return n - 4, nil
	}
	return 0, nil
}

func (d *Device) Close() error {
	d.cleanupRoutes()
	return unix.Close(d.fd)
}

// configure assigns IP and sets routes on macOS.
func (d *Device) configure() error {
	ip := d.virtualIP.String()
	ones, _ := d.subnetMask.Size()

	// Assign IP
	if err := runCmd("ifconfig", d.name, "inet", ip, ip, "netmask", net.IP(d.subnetMask).String(), "up"); err != nil {
		return fmt.Errorf("ifconfig: %w", err)
	}

	// Set MTU
	if err := runCmd("ifconfig", d.name, "mtu", fmt.Sprintf("%d", d.mtu)); err != nil {
		return fmt.Errorf("set MTU: %w", err)
	}

	// Add route for tunnel subnet
	subnet := fmt.Sprintf("%s/%d", d.virtualIP.Mask(d.subnetMask).String(), ones)
	if err := runCmd("route", "-n", "add", "-net", subnet, "-interface", d.name); err != nil {
		return fmt.Errorf("add route: %w", err)
	}

	return nil
}

// cleanupRoutes removes routes added by configure.
func (d *Device) cleanupRoutes() {
	ones, _ := d.subnetMask.Size()
	subnet := fmt.Sprintf("%s/%d", d.virtualIP.Mask(d.subnetMask).String(), ones)
	runCmd("route", "-n", "delete", "-net", subnet, "-interface", d.name)
}

func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %s", name, args, string(out))
	}
	return nil
}
