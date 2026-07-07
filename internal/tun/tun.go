//go:build windows

package tun

import (
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/tun"
)

// routeRepairInterval is how often the background goroutine checks and
// repairs broadcast routes on Windows. Windows can lose routes due to
// NLA resets, network changes, sleep/wake cycles, etc.
const routeRepairInterval = 30 * time.Second

// Device represents an active TUN device with its virtual IP (Windows).
type Device struct {
	tunDev          tun.Device
	name            string
	virtualIP       net.IP
	subnetMask      net.IPMask
	serverPublicIP  net.IP
	mtu             int
	ifIndex         uint32  // TUN adapter interface index
	luid            uint64  // TUN adapter LUID for IP Helper API calls
	physicalGateway net.IP  // physical NIC gateway IP (for server exclusion route)
	physicalIfIdx   uint32  // physical NIC interface index for IPv6 route cleanup

	maintMu     sync.Mutex
	maintStopCh chan struct{}  // closed to signal the route maintenance goroutine to stop
	maintWg     sync.WaitGroup // WaitGroup for the maintenance goroutine
}

func New(cfg Config) (*Device, error) {
	if cfg.MTU <= 0 {
		cfg.MTU = DefaultMTU
	}
	if err := validateConfig(&cfg); err != nil {
		return nil, err
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

// MTU returns the configured MTU of the TUN device.
func (d *Device) MTU() int { return d.mtu }

// BatchSize returns the maximum number of packets for batch operations.
// Windows TUN does not support true batching, so returns 1 (single-packet mode).
func (d *Device) BatchSize() int { return 1 }

// ReadBatch reads up to batchSize packets from the TUN device in a single syscall.
// Returns the number of packets read and per-packet sizes.
func (d *Device) ReadBatch(bufs [][]byte, sizes []int) (int, error) {
	n, err := d.tunDev.Read(bufs, sizes, 0)
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

// startRouteMaintenance launches a background goroutine that periodically
// re-adds broadcast routes. Windows can drop routes due to NLA resets,
// network changes, or sleep/wake cycles — this ensures they stay active.
func (d *Device) startRouteMaintenance() {
	d.maintMu.Lock()
	defer d.maintMu.Unlock()
	d.stopRouteMaintenanceLocked()
	d.maintStopCh = make(chan struct{})
	d.maintWg.Add(1)
	go func() {
		defer d.maintWg.Done()
		ticker := time.NewTicker(routeRepairInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				d.repairRoutes()
			case <-d.maintStopCh:
				return
			}
		}
	}()
	log.Printf("[tun] route maintenance started (interval=%s)", routeRepairInterval)
}

// stopRouteMaintenanceLocked is the inner implementation of stopRouteMaintenance.
// Caller must hold d.maintMu.
func (d *Device) stopRouteMaintenanceLocked() {
	if d.maintStopCh != nil {
		close(d.maintStopCh)
		d.maintWg.Wait()
		d.maintStopCh = nil
	}
}

// stopRouteMaintenance signals the maintenance goroutine to stop and waits
// for it to finish. Safe to call multiple times (idempotent).
func (d *Device) stopRouteMaintenance() {
	d.maintMu.Lock()
	defer d.maintMu.Unlock()
	d.stopRouteMaintenanceLocked()
}

// addRouteWithFallback tries IP Helper API first; on failure, falls back to netsh,
// then to route.exe.
func (d *Device) addRouteWithFallback(dest net.IP, mask net.IPMask, nextHop net.IP, metric uint32, desc string) {
	if err := addRoute(d.luid, dest, mask, nextHop, metric); err != nil {
		log.Printf("[tun] %s: API failed (%v), trying netsh", desc, err)
		if err := d.addRouteNetsh(dest, mask, nextHop, metric); err != nil {
			log.Printf("[tun] %s: netsh failed (%v), trying route add", desc, err)
			if err := d.addRouteRouteCmd(dest, mask, nextHop, metric); err != nil {
				log.Printf("[tun] %s: route add also failed: %v", desc, err)
			}
		}
	}
}

// addRouteNetsh adds a route via netsh (fallback when IP Helper API fails).
func (d *Device) addRouteNetsh(dest net.IP, mask net.IPMask, nextHop net.IP, metric uint32) error {
	prefixLen, _ := mask.Size()
	return RunCmd("netsh", "interface", "ipv4", "add", "route",
		fmt.Sprintf("%s/%d", dest, prefixLen),
		fmt.Sprintf("interface=%s", d.name),
		fmt.Sprintf("nexthop=%s", nextHop),
		fmt.Sprintf("metric=%d", metric),
	)
}

// addRouteRouteCmd adds a route via route.exe (fallback when netsh rejects the prefix).
// Unlike netsh, route.exe accepts broadcast IPs with a subnet mask (e.g. 10.10.0.255/24).
func (d *Device) addRouteRouteCmd(dest net.IP, mask net.IPMask, nextHop net.IP, metric uint32) error {
	return RunCmd("route", "add", dest.String(),
		"mask", net.IP(mask).String(),
		nextHop.String(),
		"metric", fmt.Sprintf("%d", metric),
		"if", fmt.Sprintf("%d", d.ifIndex),
	)
}

// repairRoutes re-applies broadcast routes without cleaning them first.
// The commands are idempotent — if a route already exists, the error is
// logged at debug level and ignored. This is called periodically by the
// route maintenance goroutine.
//
// Only broadcast/multicast routes are repaired. The subnet unicast route
// is excluded — if Windows drops it, the TUN device is unreachable for all
// traffic and a full reconnect is needed rather than a silent repair.
func (d *Device) repairRoutes() {
	zeroMask := net.IPMask(net.CIDRMask(32, 32))

	// Step 1: Global broadcast 255.255.255.255
	deleteRoute(d.luid, net.IPv4(255, 255, 255, 255), zeroMask, nil)
	d.addRouteWithFallback(net.IPv4(255, 255, 255, 255), zeroMask, d.virtualIP, 1, "route repair: global broadcast")

	// Step 2: Subnet broadcast
	subnet := d.virtualIP.Mask(d.subnetMask)
	subnetBroadcast := net.IP(make([]byte, 4))
	for i := 0; i < 4; i++ {
		subnetBroadcast[i] = subnet[i] | byte(^d.subnetMask[i])
	}
	deleteRoute(d.luid, subnetBroadcast, d.subnetMask, nil)
	d.addRouteWithFallback(subnetBroadcast, d.subnetMask, d.virtualIP, 1, "route repair: subnet broadcast")

	// Step 3: mDNS multicast
	d.addRouteWithFallback(net.IPv4(224, 0, 0, 251), zeroMask, d.virtualIP, 1, "route repair: mDNS")
}
