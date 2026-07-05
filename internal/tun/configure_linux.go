//go:build !windows

package tun

import (
	"fmt"
	"log"
	"net"
)

func (d *Device) configure() error {
	d.CleanupRoutes()

	ip := d.virtualIP.String()
	maskBits, _ := d.subnetMask.Size()
	subnet := d.virtualIP.Mask(d.subnetMask)

	// Assign IP
	if err := runCmd("ip", "addr", "replace", fmt.Sprintf("%s/%d", ip, maskBits), "dev", d.name); err != nil {
		return fmt.Errorf("assign IP: %w", err)
	}

	// Bring up
	if err := runCmd("ip", "link", "set", d.name, "up"); err != nil {
		return fmt.Errorf("link up: %w", err)
	}

	// Subnet route
	if err := runCmd("ip", "route", "replace", fmt.Sprintf("%s/%d", subnet, maskBits), "dev", d.name, "metric", "1"); err != nil {
		log.Printf("[tun] subnet route warning: %v", err)
	}

	// Broadcast route
	broadcast := make(net.IP, 4)
	for i := range broadcast {
		broadcast[i] = subnet[i] | byte(^d.subnetMask[i])
	}
	if err := runCmd("ip", "route", "replace", broadcast.String(), "dev", d.name, "metric", "1"); err != nil {
		log.Printf("[tun] broadcast route warning: %v", err)
	}

	// Global broadcast 255.255.255.255
	// Many LAN games (StarCraft, Age of Empires, etc.) send UDP discovery
	// broadcasts to 255.255.255.255 instead of the subnet broadcast.
	// Without this route the packet goes out the physical NIC instead of TUN.
	if err := runCmd("ip", "route", "replace", "255.255.255.255", "dev", d.name, "metric", "1"); err != nil {
		log.Printf("[tun] global broadcast route warning: %v", err)
	}

	log.Printf("[tun] configured: IP=%s/%d", ip, maskBits)
	return nil
}

func (d *Device) ReconfigureRoutes() {
	if err := d.configure(); err != nil {
		log.Printf("[tun] reconfigure routes failed: %v", err)
	}
}

func (d *Device) CleanupRoutes() {
	maskBits, _ := d.subnetMask.Size()
	subnet := d.virtualIP.Mask(d.subnetMask)

	if err := runCmd("ip", "route", "del", fmt.Sprintf("%s/%d", subnet, maskBits), "dev", d.name); err != nil {
		log.Printf("[tun] cleanup subnet route: %v", err)
	}

	broadcast := make(net.IP, 4)
	for i := range broadcast {
		broadcast[i] = subnet[i] | byte(^d.subnetMask[i])
	}
	if err := runCmd("ip", "route", "del", broadcast.String(), "dev", d.name); err != nil {
		log.Printf("[tun] cleanup broadcast route: %v", err)
	}
	if err := runCmd("ip", "route", "del", "255.255.255.255", "dev", d.name); err != nil {
		log.Printf("[tun] cleanup global broadcast route: %v", err)
	}
}
