//go:build !windows

package tun

import (
	"fmt"
	"log"
	"net"
)

const (
	// broadcastRulePriority is the priority of the ip rule that redirects
	// 255.255.255.255 (limited broadcast) to a custom routing table.
	// Must be > 0 to sit between the local table (priority 0) and the main
	// table (priority 32766).  The kernel has no local-table entry for
	// 255.255.255.255 (it uses ip_mc_output internally), so this rule is
	// the first match and routes the packet to the TUN device.
	broadcastRulePriority = 100
	// broadcastTableID is the custom routing table used for limited broadcast.
	broadcastTableID = 255
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
	//
	// On Linux the kernel handles 255.255.255.255 as a "limited broadcast"
	// in the local routing table (table local, priority 0), which sends it
	// directly out the physical NIC — normal routes in table main are never
	// consulted.  We work around this by inserting a policy rule at a lower
	// priority that redirects 255.255.255.255 to a dedicated routing table
	// pointing at the TUN device.  The rule priority (100) is above the
	// local table's implicit priority (~0) so it wins for this address.
	//
	// Add route in custom table first (safe if rule already exists)
	if err := runCmd("ip", "route", "replace", "255.255.255.255", "dev", d.name, "table", fmt.Sprint(broadcastTableID)); err != nil {
		log.Printf("[tun] global broadcast table route warning: %v", err)
	}
	// Insert policy rule: match dst 255.255.255.255 → lookup custom table
	// "ip rule add" is idempotent when the exact match already exists.
	if err := runCmd("ip", "rule", "add", "to", "255.255.255.255", "lookup", fmt.Sprint(broadcastTableID), "priority", fmt.Sprint(broadcastRulePriority)); err != nil {
		log.Printf("[tun] global broadcast rule warning: %v", err)
	}
	// Also keep a main-table route as fallback for systems where the rule
	// approach does not apply (e.g. containers without iprule support).
	if err := runCmd("ip", "route", "replace", "255.255.255.255", "dev", d.name, "metric", "1"); err != nil {
		log.Printf("[tun] global broadcast route warning: %v", err)
	}

	// mDNS multicast
	if err := runCmd("ip", "route", "replace", "224.0.0.251", "dev", d.name, "metric", "1"); err != nil {
		log.Printf("[tun] mDNS route warning: %v", err)
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
	if err := runCmd("ip", "route", "del", "255.255.255.255", "table", fmt.Sprint(broadcastTableID)); err != nil {
		log.Printf("[tun] cleanup global broadcast table route: %v", err)
	}
	if err := runCmd("ip", "rule", "del", "to", "255.255.255.255", "lookup", fmt.Sprint(broadcastTableID), "priority", fmt.Sprint(broadcastRulePriority)); err != nil {
		log.Printf("[tun] cleanup global broadcast rule: %v", err)
	}
	if err := runCmd("ip", "route", "del", "224.0.0.251", "dev", d.name); err != nil {
		log.Printf("[tun] cleanup mDNS route: %v", err)
	}
}
