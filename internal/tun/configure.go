//go:build windows

// configure.go — 替换 tun.go 中的 configure() 方法。
//
// 改动点：
//   - Step 2/3: 用 IP Helper API 替代 PowerShell 设置 metric（保留 PS 回退）
//   - verifyMetric 改为检查 AutomaticMetric 是否禁用（根因），而非检查 metric 值（会被覆盖）
//   - 同时禁用所有物理网卡的 AutomaticMetric，防止 Windows NLA 服务回退

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

	mask := net.IP(d.subnetMask).String()
	ip := d.virtualIP.String()

	// ── Step 1: 分配静态 IP ──
	if err := RunCmd("netsh", "interface", "ip", "set", "address",
		fmt.Sprintf("name=%s", d.name),
		"static", ip, mask); err != nil {
		return fmt.Errorf("assign IP: %w", err)
	}

	// ── Step 2: 禁用 AutomaticMetric ──
	// 等待 TUN 适配器完全初始化（刚创建的适配器 API 调用可能返回 ERROR_INVALID_PARAMETER）
	time.Sleep(1 * time.Second)

	// 优先用 IP Helper API（快速、可靠），失败则回退 PowerShell。
	if err := d.applyMetricAPI(); err != nil {
		log.Printf("[tun] IP Helper API failed (%v), trying PowerShell", err)
		d.applyMetricPowerShell()
	}

	// ── Step 3: 验证 + 重试 ──
	if !checkAutoMetricDisabled(d.name) {
		time.Sleep(2 * time.Second)
		if err := d.applyMetricAPI(); err != nil {
			d.applyMetricPowerShell()
		}
		if checkAutoMetricDisabled(d.name) {
			log.Printf("[tun] metric=1 verified")
		} else {
			log.Printf("[tun] metric verification inconclusive (netsh set OK, broadcast routing should work)")
		}
	}

	// ── Step 4: 子网路由 ──
	subnet := d.virtualIP.Mask(d.subnetMask)
	maskBits, _ := d.subnetMask.Size()
	if err := RunCmd("route", "add",
		fmt.Sprintf("%s/%d", subnet, maskBits), "mask", mask, ip, "metric", "1"); err != nil {
		log.Printf("[tun] subnet route warning: %v", err)
	}

	// ── Step 5: 全局广播 255.255.255.255 ──
	// 游戏（如星际争霸）发 UDP 广播到 255.255.255.255:6112 发现局域网。
	if err := RunCmd("route", "add",
		"255.255.255.255", "mask", "255.255.255.255", ip, "metric", "1"); err != nil {
		log.Printf("[tun] broadcast route warning: %v", err)
	}

	// ── Step 6: 子网广播（如 10.10.0.255）──
	subnetBroadcast := net.IP(make([]byte, 4))
	for i := 0; i < 4; i++ {
		subnetBroadcast[i] = subnet[i] | ^d.subnetMask[i]
	}
	if err := RunCmd("route", "add",
		subnetBroadcast.String(), "mask", mask, ip, "metric", "1"); err != nil {
		log.Printf("[tun] subnet broadcast route warning: %v", err)
	}

	// ── Step 7: mDNS 组播 224.0.0.251 ──
	if err := RunCmd("route", "add",
		"224.0.0.251", "mask", "255.255.255.255", ip, "metric", "1"); err != nil {
		log.Printf("[tun] mDNS route warning: %v", err)
	}

	// ── Step 8: 隧道服务器排除路由 ──
	// 隧道服务器必须走物理网卡，否则 UDP 封装的隧道流量会回环进 TUN。
	// IPv4: 添加 /32 主机路由指向物理网关。
	// IPv6: 使用 netsh interface ipv6 添加 /128 主机路由。
	if d.serverPublicIP != nil {
		isv6 := d.serverPublicIP.To4() == nil
		prefix := "0.0.0.0/0"
		if isv6 {
			prefix = "::/0"
		}
		gw := d.detectPhysicalGatewayForPrefix(prefix)

		// Fallback: if IPv6 gateway not found, try IPv4 gateway.
		// On some Windows systems the IPv6 default route may not appear
		// in Get-NetRoute, but the IPv4 gateway can still route IPv6 traffic
		// on dual-stack networks.
		if gw == "" && isv6 {
			log.Printf("[tun] IPv6 gateway not found, trying IPv4 gateway as fallback")
			gw = d.detectPhysicalGatewayForPrefix("0.0.0.0/0")
		}

		if gw != "" {
			// Validate gateway address family matches the route we're adding.
			// An IPv4 nexthop on an IPv6 route (or vice versa) will fail
			// with "The parameter is incorrect" on Windows.
			gwIP := net.ParseIP(gw)
			if isv6 && gwIP != nil && gwIP.To4() != nil {
				log.Printf("[tun] WARNING: detected gateway %s is IPv4 but server is IPv6, skipping exclusion route", gw)
				log.Printf("[tun] TIP: add a static IPv6 route manually: netsh interface ipv6 add route %s/128 <interface> nexthop=<ipv6-gw>", d.serverPublicIP)
			} else if !isv6 && gwIP != nil && gwIP.To4() == nil {
				log.Printf("[tun] WARNING: detected gateway %s is IPv6 but server is IPv4, skipping exclusion route", gw)
			} else {
				d.physicalGateway = gw
				serverIP := d.serverPublicIP.String()
				if isv6 {
					// netsh interface ipv6 add route <addr>/128 interface=<phyIdx> nexthop=<gw> metric=1
					ifaceIdx := d.getPhysicalInterfaceIndex("::/0")
					if ifaceIdx == 0 {
						ifaceIdx = d.getPhysicalInterfaceIndex("0.0.0.0/0")
					}
					if ifaceIdx == 0 {
						ifaceIdx = d.getInterfaceIndex()
						log.Printf("[tun] WARNING: using TUN interface for IPv6 exclusion route (physical NIC not detected)")
					}
					d.physicalIfIdx = ifaceIdx
					if err := RunCmd("netsh", "interface", "ipv6", "add", "route",
						fmt.Sprintf("%s/128", serverIP), fmt.Sprintf("interface=%d", ifaceIdx),
						fmt.Sprintf("nexthop=%s", gw), "metric=1"); err != nil {
						log.Printf("[tun] server exclusion route warning: %v", err)
					} else {
						log.Printf("[tun] server exclusion (IPv6): %s → %s via NIC idx=%d", serverIP, gw, ifaceIdx)
					}
				} else {
					if err := RunCmd("route", "add",
						serverIP, "mask", "255.255.255.255", gw, "metric", "1"); err != nil {
						log.Printf("[tun] server exclusion route warning: %v", err)
					} else {
						log.Printf("[tun] server exclusion: %s → %s (physical NIC)", serverIP, gw)
					}
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

	// ── Step 9: 网络配置文件设为 Private ──
	//
	// 注意：不添加 0.0.0.0/0 默认路由。
	// 游戏流量目标是 10.10.0.x 虚拟 IP，已被 Step 4 子网路由覆盖。
	// 广播/组播流量已被 Step 5-7 覆盖。
	// 添加默认路由会劫持用户全部网络流量（网页、DNS 等），存在安全隐患：
	// 若服务器被入侵，中间人可嗅探所有流量。且非隧道流量在 routePacket()
	// 中会被静默丢弃，既无用又破坏用户正常上网。
	if err := RunCmd("powershell", "-NoProfile", "-Command",
		fmt.Sprintf("Set-NetConnectionProfile -InterfaceAlias '%s' -NetworkCategory Private", d.name)); err != nil {
		log.Printf("[tun] network category warning: %v", err)
	}

	log.Printf("[tun] configured: IP=%s/%d, subnet route only (no default route)", ip, maskBits)

	return nil
}

// ReconfigureRoutes re-applies routes without recreating the TUN device.
// Called on reconnect — routes may have been modified by the OS during
// disconnection (e.g. Windows Network Location Awareness service).
// Cleans existing routes first to avoid conflicts with stale entries.
func (d *Device) ReconfigureRoutes() {
	// Remove stale routes before re-adding. Windows route add fails silently
	// if the route already exists with a different metric, leaving the old
	// (potentially wrong) metric in effect.
	d.CleanupRoutes()

	mask := net.IP(d.subnetMask).String()
	ip := d.virtualIP.String()

	// Subnet route
	subnet := d.virtualIP.Mask(d.subnetMask)
	maskBits, _ := d.subnetMask.Size()
	RunCmd("route", "add",
		fmt.Sprintf("%s/%d", subnet, maskBits), "mask", mask, ip, "metric", "1")

	// Global broadcast
	RunCmd("route", "add",
		"255.255.255.255", "mask", "255.255.255.255", ip, "metric", "1")

	// Subnet broadcast
	subnetBroadcast := net.IP(make([]byte, 4))
	for i := 0; i < 4; i++ {
		subnetBroadcast[i] = subnet[i] | ^d.subnetMask[i]
	}
	RunCmd("route", "add",
		subnetBroadcast.String(), "mask", mask, ip, "metric", "1")

	// mDNS multicast
	RunCmd("route", "add",
		"224.0.0.251", "mask", "255.255.255.255", ip, "metric", "1")

	// Server exclusion route
	if d.serverPublicIP != nil {
		isv6 := d.serverPublicIP.To4() == nil
		prefix := "0.0.0.0/0"
		if isv6 {
			prefix = "::/0"
		}
		gw := d.detectPhysicalGatewayForPrefix(prefix)

		// Fallback: if IPv6 gateway not found, try IPv4 gateway.
		if gw == "" && isv6 {
			gw = d.detectPhysicalGatewayForPrefix("0.0.0.0/0")
		}

		if gw != "" {
			gwIP := net.ParseIP(gw)
			// Validate gateway address family matches the route.
			if isv6 && gwIP != nil && gwIP.To4() != nil {
				log.Printf("[tun] WARNING: gateway %s is IPv4 but server is IPv6, skipping exclusion route", gw)
			} else if !isv6 && gwIP != nil && gwIP.To4() == nil {
				log.Printf("[tun] WARNING: gateway %s is IPv6 but server is IPv4, skipping exclusion route", gw)
			} else {
				d.physicalGateway = gw
				serverIP := d.serverPublicIP.String()
				if isv6 {
					ifaceIdx := d.getPhysicalInterfaceIndex("::/0")
					if ifaceIdx == 0 {
						ifaceIdx = d.getPhysicalInterfaceIndex("0.0.0.0/0")
					}
					if ifaceIdx == 0 {
						ifaceIdx = d.getInterfaceIndex()
					}
					d.physicalIfIdx = ifaceIdx
					RunCmd("netsh", "interface", "ipv6", "add", "route",
						fmt.Sprintf("%s/128", serverIP), fmt.Sprintf("interface=%d", ifaceIdx),
						fmt.Sprintf("nexthop=%s", gw), "metric=1")
				} else {
					RunCmd("route", "add",
						serverIP, "mask", "255.255.255.255", gw, "metric", "1")
				}
			}
		}
	}

	log.Printf("[tun] routes reconfigured on reconnect")
}

// applyMetricAPI 通过 IP Helper API 禁用 TUN + 物理网卡的 AutomaticMetric。
func (d *Device) applyMetricAPI() error {
	idx, luid, err := findAdapter(d.name)
	if err != nil {
		return fmt.Errorf("find TUN: %w", err)
	}
	log.Printf("[tun] TUN adapter: idx=%d luid=%d", idx, luid)

	if err := setMetricAPI(idx, luid); err != nil {
		return fmt.Errorf("set TUN: %w", err)
	}
	log.Printf("[tun] TUN AutomaticMetric disabled (IP Helper API)")

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

// detectPhysicalGatewayForPrefix finds the default gateway of the physical NIC
// for the given address family. prefix should be "0.0.0.0/0" for IPv4 or "::/0" for IPv6.
func (d *Device) detectPhysicalGatewayForPrefix(prefix string) string {
	out, err := runCmdOutput("powershell", "-NoProfile", "-Command",
		fmt.Sprintf(
			"Get-NetRoute -DestinationPrefix '%s' | Where-Object { $_.InterfaceAlias -ne '%s' } | Sort-Object RouteMetric | Select-Object -First 1 -ExpandProperty NextHop",
			prefix, d.name))
	if err != nil {
		log.Printf("[tun] gateway detection failed (%s): %v", prefix, err)
		return ""
	}
	gw := strings.TrimSpace(out)
	if gw != "" {
		log.Printf("[tun] detected physical gateway (%s): %s", prefix, gw)
		return gw
	}
	return ""
}

// getInterfaceIndex returns the TUN adapter's interface index for netsh commands.
func (d *Device) getInterfaceIndex() int {
	out, err := runCmdOutput("powershell", "-NoProfile", "-Command",
		fmt.Sprintf("(Get-NetAdapter -InterfaceAlias '%s').ifIndex", d.name))
	if err != nil {
		log.Printf("[tun] get interface index failed: %v", err)
		return 0
	}
	var idx int
	fmt.Sscanf(strings.TrimSpace(out), "%d", &idx)
	return idx
}

// getPhysicalInterfaceIndex returns the interface index of the physical NIC
// that holds the default route for the given prefix.
func (d *Device) getPhysicalInterfaceIndex(prefix string) int {
	out, err := runCmdOutput("powershell", "-NoProfile", "-Command",
		fmt.Sprintf(
			"(Get-NetRoute -DestinationPrefix '%s' | Where-Object { $_.InterfaceAlias -ne '%s' } | Sort-Object RouteMetric | Select-Object -First 1).InterfaceIndex",
			prefix, d.name))
	if err != nil {
		return 0
	}
	var idx int
	fmt.Sscanf(strings.TrimSpace(out), "%d", &idx)
	return idx
}

// CleanupRoutes removes routes added by configure().
// Called when the TUN device is being destroyed.
func (d *Device) CleanupRoutes() {
	// Remove server exclusion route
	if d.serverPublicIP != nil && d.physicalGateway != "" {
		if d.serverPublicIP.To4() == nil {
			// IPv6: use netsh to delete the host route
			ifaceIdx := d.physicalIfIdx
			if ifaceIdx == 0 {
				ifaceIdx = d.getInterfaceIndex()
			}
			RunCmd("netsh", "interface", "ipv6", "delete", "route",
				fmt.Sprintf("%s/128", d.serverPublicIP.String()),
				fmt.Sprintf("interface=%d", ifaceIdx))
		} else {
			RunCmd("route", "delete", d.serverPublicIP.String())
		}
	}

	// Remove broadcast routes
	RunCmd("route", "delete", "255.255.255.255")
	RunCmd("route", "delete", "224.0.0.251")

	// Remove subnet route
	subnet := d.virtualIP.Mask(d.subnetMask)
	maskBits, _ := d.subnetMask.Size()
	RunCmd("route", "delete", fmt.Sprintf("%s/%d", subnet, maskBits))

	// Remove subnet broadcast
	subnetBroadcast := net.IP(make([]byte, 4))
	for i := 0; i < 4; i++ {
		subnetBroadcast[i] = subnet[i] | ^d.subnetMask[i]
	}
	RunCmd("route", "delete", subnetBroadcast.String())

	log.Printf("[tun] routes cleaned up")
}
