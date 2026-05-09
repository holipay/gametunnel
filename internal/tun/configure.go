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
	mask := net.IP(d.subnetMask).String()
	ip := d.virtualIP.String()

	// ── Step 1: 分配静态 IP ──
	if err := RunCmd("netsh", "interface", "ip", "set", "address",
		fmt.Sprintf("name=%s", d.name),
		"static", ip, mask); err != nil {
		return fmt.Errorf("assign IP: %w", err)
	}

	// ── Step 2: 禁用 AutomaticMetric ──
	// 优先用 IP Helper API（快速、可靠），失败则回退 PowerShell。
	if err := d.applyMetricAPI(); err != nil {
		log.Printf("[tun] IP Helper API failed (%v), trying PowerShell", err)
		d.applyMetricPowerShell()
	}

	// ── Step 3: 验证 + 重试 ──
	if !checkAutoMetricDisabled(d.name) {
		time.Sleep(500 * time.Millisecond)
		if err := d.applyMetricAPI(); err != nil {
			d.applyMetricPowerShell()
		}
		if checkAutoMetricDisabled(d.name) {
			log.Printf("[tun] AutomaticMetric disabled (retry OK)")
		} else {
			log.Printf("[tun] WARNING: could not disable AutomaticMetric, broadcast routing may be affected")
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

	// ── Step 8: 网络配置文件设为 Private ──
	if err := RunCmd("powershell", "-NoProfile", "-Command",
		fmt.Sprintf("Set-NetConnectionProfile -InterfaceAlias '%s' -NetworkCategory Private", d.name)); err != nil {
		log.Printf("[tun] network category warning: %v", err)
	}

	return nil
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
