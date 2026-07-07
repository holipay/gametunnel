//go:build windows

// configure.go — replaces the configure() method from tun.go.
//
// Changes compared to the Linux version:
//   - Steps 2/3: use IP Helper API instead of PowerShell for metric (keep PS fallback)
//   - verifyMetric checks if AutomaticMetric is disabled (root cause), not the metric value
//   - disable AutomaticMetric on all physical NICs to prevent Windows NLA rollback

package tun

import (
	"fmt"
	"log"
	"net"
	"strings"
	"time"
)

// configure 分配 IP、设置路由、确保广播走 TUN。
func (d *Device) configure() error {
	// 幂等化：先清理可能残留的旧路由（程序崩溃后不会执行 CleanupRoutes）。
	d.CleanupRoutes()

	maskBits, _ := d.subnetMask.Size()
	zeroMask := net.IPMask(net.CIDRMask(32, 32))

	// ── Step 1: 查找适配器 ──
	// 等待 TUN 适配器完全初始化（刚创建的适配器 API 调用可能返回 ERROR_INVALID_PARAMETER）
	time.Sleep(1 * time.Second)

	idx, luid, err := findAdapter(d.name)
	if err != nil {
		return fmt.Errorf("find TUN adapter: %w", err)
	}
	d.ifIndex = idx
	d.luid = luid
	log.Printf("[tun] TUN adapter: idx=%d luid=%d", idx, luid)

	// ── Step 2: 分配静态 IP ──
	if err := addIPAddress(luid, idx, d.virtualIP, d.subnetMask); err != nil {
		// Fallback: netsh for systems where CreateUnicastIpAddressEntry fails
		log.Printf("[tun] addIPAddress failed (%v), trying netsh", err)
		mask := net.IP(d.subnetMask).String()
		if err := RunCmd("netsh", "interface", "ip", "set", "address",
			fmt.Sprintf("name=%s", d.name),
			"static", d.virtualIP.String(), mask); err != nil {
			return fmt.Errorf("assign IP: %w", err)
		}
	}

	// ── Step 3: 禁用 AutomaticMetric ──
	if err := setMetricAPI(luid); err != nil {
		log.Printf("[tun] IP Helper metric failed (%v), trying PowerShell", err)
		d.applyMetricPowerShell()
	}

	// ── Step 4: 验证 + 重试 ──
	if !checkAutoMetricDisabled(luid) {
		time.Sleep(2 * time.Second)
		if err := setMetricAPI(luid); err != nil {
			d.applyMetricPowerShell()
		}
		if checkAutoMetricDisabled(luid) {
			log.Printf("[tun] metric=1 verified")
		} else {
			log.Printf("[tun] metric verification inconclusive")
		}
	}

	// 同时禁用所有物理网卡的 AutomaticMetric
	disableAllPhysicalAutoMetric(d.name)

	// ── Step 5: 子网路由 ──
	// CleanupRoutes() 在前面已删除旧路由，但 WireGuard 内核驱动会在
	// TUN 创建时重新添加子网路由（metric 257），需要再删一次。
	deleteRoute(luid, d.virtualIP.Mask(d.subnetMask), d.subnetMask, nil)
	d.addRouteWithFallback(d.virtualIP.Mask(d.subnetMask), d.subnetMask, d.virtualIP, 1, "subnet route")

	// ── Step 6: 全局广播 255.255.255.255 ──
	// 游戏（如星际争霸）发 UDP 广播到 255.255.255.255:6112 发现局域网。
	bcast := net.IPv4(255, 255, 255, 255)
	deleteRoute(luid, bcast, zeroMask, nil)
	d.addRouteWithFallback(bcast, zeroMask, d.virtualIP, 1, "broadcast route")

	// ── Step 7: 子网广播（如 10.10.0.255）──
	subnetBroadcast := net.IP(make([]byte, 4))
	subnet := d.virtualIP.Mask(d.subnetMask)
	for i := 0; i < 4; i++ {
		subnetBroadcast[i] = subnet[i] | byte(^d.subnetMask[i])
	}
	deleteRoute(luid, subnetBroadcast, d.subnetMask, nil)
	d.addRouteWithFallback(subnetBroadcast, d.subnetMask, d.virtualIP, 1, "subnet broadcast route")

	// ── Step 8: mDNS 组播 224.0.0.251 ──
	mdns := net.IPv4(224, 0, 0, 251)
	d.addRouteWithFallback(mdns, zeroMask, d.virtualIP, 1, "mDNS route")

	// ── Step 9: 隧道服务器排除路由 ──
	// 隧道服务器必须走物理网卡，否则 UDP 封装的隧道流量会回环进 TUN。
	if d.serverPublicIP != nil {
		isv6 := d.serverPublicIP.To4() == nil
		prefix := "0.0.0.0/0"
		if isv6 {
			prefix = "::/0"
		}

		gw, phyIfIdx, _ := detectPhysicalGateway(prefix, d.name)

		// Fallback: if IPv6 gateway not found, try IPv4 gateway.
		if gw == nil && isv6 {
			log.Printf("[tun] IPv6 gateway not found, trying IPv4 gateway as fallback")
			gw, phyIfIdx, _ = detectPhysicalGateway("0.0.0.0/0", d.name)
		}

		if gw != nil {
			// Validate gateway address family matches the route we're adding.
			if isv6 && gw.To4() != nil {
				log.Printf("[tun] WARNING: detected gateway %s is IPv4 but server is IPv6, skipping exclusion route", gw)
				log.Printf("[tun] TIP: add a static IPv6 route manually: netsh interface ipv6 add route %s/128 <interface> nexthop=<ipv6-gw>", d.serverPublicIP)
			} else if !isv6 && gw.To4() == nil {
				log.Printf("[tun] WARNING: detected gateway %s is IPv6 but server is IPv4, skipping exclusion route", gw)
			} else {
				d.physicalGateway = gw
				d.physicalIfIdx = phyIfIdx
				serverIP := d.serverPublicIP
				if isv6 {
					// IPv6: use netsh (no clean API for route-by-interface-index)
					if phyIfIdx == 0 {
						phyIfIdx = d.ifIndex
						log.Printf("[tun] WARNING: using TUN interface for IPv6 exclusion route (physical NIC not detected)")
					}
					if err := RunCmd("netsh", "interface", "ipv6", "add", "route",
						fmt.Sprintf("%s/128", serverIP), fmt.Sprintf("interface=%d", phyIfIdx),
						fmt.Sprintf("nexthop=%s", gw), "metric=1"); err != nil {
						log.Printf("[tun] server exclusion route warning: %v", err)
					} else {
						log.Printf("[tun] server exclusion (IPv6): %s → %s via NIC idx=%d", serverIP, gw, phyIfIdx)
					}
				} else {
					// IPv4: use IP Helper API with netsh fallback
					d.addRouteWithFallback(serverIP, zeroMask, gw, 1, "server exclusion route")
				}
			}
		} else {
			log.Printf("[tun] WARNING: cannot detect physical gateway, server route exclusion skipped")
			if isv6 {
				log.Printf("[tun] TIP: ensure your network adapter has a default gateway configured")
				log.Printf("[tun] TIP: or add a static route: netsh interface ipv6 add route %s/128 <interface> nexthop=<gw>", d.serverPublicIP)
			}
		}
	}

	// ── Step 10: 网络配置文件设为 Private ──
	//
	// 注意：不添加 0.0.0.0/0 默认路由。
	// 游戏流量目标是 10.10.0.x 虚拟 IP，已被 Step 5 子网路由覆盖。
	// 广播/组播流量已被 Step 6-8 覆盖。
	// 添加默认路由会劫持用户全部网络流量（网页、DNS 等），存在安全隐患。
	if err := RunCmd("powershell", "-NoProfile", "-Command",
		fmt.Sprintf("Set-NetConnectionProfile -InterfaceAlias '%s' -NetworkCategory Private", d.name)); err != nil {
		log.Printf("[tun] network category warning: %v", err)
	}

	// Start background route repair. Windows can drop routes at any time
	// due to NLA resets, network changes, or sleep/wake cycles.
	d.startRouteMaintenance()

	log.Printf("[tun] configured: IP=%s/%d, subnet route only (no default route)", d.virtualIP, maskBits)

	return nil
}

// ReconfigureRoutes re-applies routes without recreating the TUN device.
// Called on reconnect — routes may have been modified by the OS during
// disconnection (e.g. Windows Network Location Awareness service).
// Cleans existing routes first to avoid conflicts with stale entries.
func (d *Device) ReconfigureRoutes() {
	d.CleanupRoutes()

	zeroMask := net.IPMask(net.CIDRMask(32, 32))
	// Subnet route
	deleteRoute(d.luid, d.virtualIP.Mask(d.subnetMask), d.subnetMask, nil)
	d.addRouteWithFallback(d.virtualIP.Mask(d.subnetMask), d.subnetMask, d.virtualIP, 1, "ReconfigureRoutes: subnet route")

	// Global broadcast 255.255.255.255
	bcast := net.IPv4(255, 255, 255, 255)
	deleteRoute(d.luid, bcast, zeroMask, nil)
	d.addRouteWithFallback(bcast, zeroMask, d.virtualIP, 1, "ReconfigureRoutes: broadcast route")

	// Subnet broadcast
	subnet := d.virtualIP.Mask(d.subnetMask)
	subnetBroadcast := net.IP(make([]byte, 4))
	for i := 0; i < 4; i++ {
		subnetBroadcast[i] = subnet[i] | byte(^d.subnetMask[i])
	}
	deleteRoute(d.luid, subnetBroadcast, d.subnetMask, nil)
	d.addRouteWithFallback(subnetBroadcast, d.subnetMask, d.virtualIP, 1, "ReconfigureRoutes: subnet broadcast route")

	// mDNS multicast
	mdns := net.IPv4(224, 0, 0, 251)
	d.addRouteWithFallback(mdns, zeroMask, d.virtualIP, 1, "ReconfigureRoutes: mDNS route")

	// Server exclusion route
	if d.serverPublicIP != nil {
		isv6 := d.serverPublicIP.To4() == nil
		prefix := "0.0.0.0/0"
		if isv6 {
			prefix = "::/0"
		}

		gw, phyIfIdx, _ := detectPhysicalGateway(prefix, d.name)
		if gw == nil && isv6 {
			gw, phyIfIdx, _ = detectPhysicalGateway("0.0.0.0/0", d.name)
		}

		if gw != nil {
			if isv6 && gw.To4() != nil {
				log.Printf("[tun] WARNING: gateway %s is IPv4 but server is IPv6, skipping exclusion route", gw)
			} else if !isv6 && gw.To4() == nil {
				log.Printf("[tun] WARNING: gateway %s is IPv6 but server is IPv4, skipping exclusion route", gw)
			} else {
				d.physicalGateway = gw
				d.physicalIfIdx = phyIfIdx
				if isv6 {
					if phyIfIdx == 0 {
						phyIfIdx = d.ifIndex
					}
					RunCmd("netsh", "interface", "ipv6", "add", "route",
						fmt.Sprintf("%s/128", d.serverPublicIP), fmt.Sprintf("interface=%d", phyIfIdx),
						fmt.Sprintf("nexthop=%s", gw), "metric=1")
				} else {
					// IPv4: use IP Helper API with netsh fallback
					d.addRouteWithFallback(d.serverPublicIP, zeroMask, gw, 1, "ReconfigureRoutes: server exclusion route")
				}
			}
		} else {
			d.physicalGateway = nil
		}
	}

	d.startRouteMaintenance()
	log.Printf("[tun] routes reconfigured on reconnect")
}

// applyMetricAPI 通过 IP Helper API 禁用 TUN + 物理网卡的 AutomaticMetric。
func (d *Device) applyMetricAPI() error {
	idx, luid, err := findAdapter(d.name)
	if err != nil {
		return fmt.Errorf("find TUN: %w", err)
	}
	log.Printf("[tun] TUN adapter: idx=%d luid=%d", idx, luid)

	if err := setMetricAPI(luid); err != nil {
		return fmt.Errorf("set TUN: %w", err)
	}
	log.Printf("[tun] AutomaticMetric disabled (IP Helper API)")

	// 同时禁用所有物理网卡
	disableAllPhysicalAutoMetric(d.name)
	return nil
}

// applyMetricPowerShell 是 PowerShell 回退方案。
func (d *Device) applyMetricPowerShell() {
	// TUN: 禁用自动 metric + 设为 1
	psTUN := fmt.Sprintf(
		"Set-NetIPInterface -InterfaceAlias '%s' -AutomaticMetric Disabled -InterfaceMetric 1 -ErrorAction Stop",
		d.name)
	if err := RunCmd("powershell", "-NoProfile", "-Command", psTUN); err != nil {
		log.Printf("[tun] PS TUN metric: %v", err)
	}

	// 物理网卡: 禁用自动 metric + 设为 100
	out, err := runCmdOutput("powershell", "-NoProfile", "-Command",
		fmt.Sprintf("Get-NetAdapter | Where-Object { $_.Status -eq 'Up' -and $_.Name -ne '%s' -and $_.InterfaceDescription -notmatch 'Loopback' } | Select-Object -ExpandProperty Name", d.name))
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		nic := strings.TrimSpace(line)
		if nic == "" || nic == d.name {
			continue
		}
		RunCmd("powershell", "-NoProfile", "-Command",
			fmt.Sprintf("Set-NetIPInterface -InterfaceAlias '%s' -AutomaticMetric Disabled -InterfaceMetric 100 -ErrorAction SilentlyContinue", nic))
	}
}

// CleanupRoutes removes routes added by configure().
// Called when the TUN device is being destroyed.
func (d *Device) CleanupRoutes() {
	d.stopRouteMaintenance()

	zeroMask := net.IPMask(net.CIDRMask(32, 32))
	luid := d.luid

	// Remove server exclusion route
	if d.serverPublicIP != nil && d.physicalGateway != nil {
		if d.serverPublicIP.To4() == nil {
			// IPv6: use netsh (no clean API for route-by-interface-index)
			ifaceIdx := d.physicalIfIdx
			if ifaceIdx == 0 {
				ifaceIdx = d.ifIndex
			}
			RunCmd("netsh", "interface", "ipv6", "delete", "route",
				fmt.Sprintf("%s/128", d.serverPublicIP.String()),
				fmt.Sprintf("interface=%d", ifaceIdx))
		} else {
			deleteRoute(luid, d.serverPublicIP, zeroMask, d.physicalGateway)
		}
	}

	// Remove broadcast routes
	deleteRoute(luid, net.IPv4(255, 255, 255, 255), zeroMask, nil)

	// Remove mDNS multicast
	deleteRoute(luid, net.IPv4(224, 0, 0, 251), zeroMask, nil)

	// Remove subnet route
	subnet := d.virtualIP.Mask(d.subnetMask)
	deleteRoute(luid, subnet, d.subnetMask, nil)

	// Remove subnet broadcast
	subnetBroadcast := net.IP(make([]byte, 4))
	for i := 0; i < 4; i++ {
		subnetBroadcast[i] = subnet[i] | byte(^d.subnetMask[i])
	}
	deleteRoute(luid, subnetBroadcast, d.subnetMask, nil)

	log.Printf("[tun] routes cleaned up")
}
