//go:build windows

// metric_windows.go — 使用 Windows IP Helper API 直接管理网卡 metric。
//
// 改动点：
//   - findAdapter 使用 ConvertInterfaceIndexToLuid 替代 GetIpInterfaceEntry
//     （GetIpInterfaceEntry 在 wintun 适配器上读不到 UseAutomaticMetric）
//   - checkAutoMetricDisabled 使用 PowerShell 验证（IP Helper API 对 wintun 不可靠）

package tun

import (
	"fmt"
	"log"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modIphlpapi = syscall.NewLazyDLL("iphlpapi.dll")

	procGetAdaptersAddresses       = modIphlpapi.NewProc("GetAdaptersAddresses")
	procConvertInterfaceIndexToLuid = modIphlpapi.NewProc("ConvertInterfaceIndexToLuid")
)

const (
	gaaFlagSkipUnicast      = 0x0001
	gaaFlagSkipAnycast      = 0x0002
	gaaFlagSkipMulticast    = 0x0004
	gaaFlagSkipDNS          = 0x0008
	gaaFlagSkipFriendlyName = 0x0020
)

// ipAdapterAddresses 最小布局，用于枚举网卡。
type ipAdapterAddresses struct {
	Length          uint32
	IfIndex         uint32
	Next            *ipAdapterAddresses
	AdapterName     *byte
	FirstUnicast    uintptr
	FirstAnycast    uintptr
	FirstMulticast  uintptr
	FirstDNS        uintptr
	DnsSuffix       *uint16
	Description     *uint16
	FriendlyName    *uint16
	PhysicalAddr    [8]byte
	PhysicalAddrLen uint32
	Flags           uint32
	Mtu             uint32
	IfType          uint32
	OperStatus      uint32 // 1 = IfOperStatusUp
}

// setMetricAPI 通过 PowerShell 禁用指定网卡的 AutomaticMetric 并设置 metric 值。
//
// netsh interface ip set interface 对 wintun 虚拟适配器无效，
// 必须用 PowerShell Set-NetIPInterface 才能真正生效。
func setMetricAPI(ifIndex uint32) error {
	name, err := findAdapterNameByIndex(ifIndex)
	if err != nil {
		return fmt.Errorf("find adapter name: %w", err)
	}

	// PowerShell: 禁用 AutomaticMetric + 设置 metric=1
	ps := fmt.Sprintf(
		"Set-NetIPInterface -InterfaceAlias '%s' -AutomaticMetric Disabled -InterfaceMetric 1 -ErrorAction Stop",
		name)
	if err := RunCmd("powershell", "-NoProfile", "-Command", ps); err != nil {
		return fmt.Errorf("Set-NetIPInterface: %w", err)
	}

	log.Printf("[tun] metric=1 set via PowerShell: %s (idx=%d)", name, ifIndex)
	return nil
}

// findAdapterNameByIndex 通过接口索引查找 FriendlyName。
func findAdapterNameByIndex(targetIdx uint32) (string, error) {
	var bufLen uint32 = 15000
	buf := make([]byte, bufLen)

	r1, _, _ := procGetAdaptersAddresses.Call(
		uintptr(syscall.AF_INET),
		uintptr(gaaFlagSkipUnicast|gaaFlagSkipAnycast|gaaFlagSkipMulticast|gaaFlagSkipDNS),
		0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&bufLen)),
	)
	if r1 != 0 {
		return "", fmt.Errorf("GetAdaptersAddresses: ret=%d", r1)
	}

	p := (*ipAdapterAddresses)(unsafe.Pointer(&buf[0]))
	for p != nil {
		if p.IfIndex == targetIdx {
			return windows.UTF16PtrToString(p.FriendlyName), nil
		}
		p = p.Next
	}
	return "", fmt.Errorf("adapter with index %d not found", targetIdx)
}

// findAdapter 按 FriendlyName 查找网卡，返回 ifIndex 和 LUID。
// 使用 ConvertInterfaceIndexToLuid 获取 LUID（对 wintun 等虚拟适配器可靠）。
func findAdapter(name string) (ifIndex uint32, luid uint64, err error) {
	var bufLen uint32 = 15000
	buf := make([]byte, bufLen)

	for i := 0; i < 3; i++ {
		r1, _, _ := procGetAdaptersAddresses.Call(
			uintptr(syscall.AF_INET),
			uintptr(gaaFlagSkipUnicast|gaaFlagSkipAnycast|gaaFlagSkipMulticast|gaaFlagSkipDNS),
			0,
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(unsafe.Pointer(&bufLen)),
		)
		if r1 == 0 {
			break
		}
		if r1 != 111 /* ERROR_BUFFER_OVERFLOW */ {
			return 0, 0, fmt.Errorf("GetAdaptersAddresses: ret=%d", r1)
		}
		buf = make([]byte, bufLen)
	}

	p := (*ipAdapterAddresses)(unsafe.Pointer(&buf[0]))
	for p != nil {
		if windows.UTF16PtrToString(p.FriendlyName) == name {
			// 用 ConvertInterfaceIndexToLuid 获取 LUID（不依赖 GetIpInterfaceEntry，wintun 兼容）
			r1, _, _ := procConvertInterfaceIndexToLuid.Call(
				uintptr(p.IfIndex),
				uintptr(unsafe.Pointer(&luid)),
			)
			if r1 == 0 {
				return p.IfIndex, luid, nil
			}
			return 0, 0, fmt.Errorf("ConvertInterfaceIndexToLuid(idx=%d): ret=%d", p.IfIndex, r1)
		}
		p = p.Next
	}
	return 0, 0, fmt.Errorf("adapter %q not found", name)
}

// checkAutoMetricDisabled 检查网卡的 AutomaticMetric 是否已禁用且 metric=1。
func checkAutoMetricDisabled(name string) bool {
	// 检查 AutomaticMetric 是否已禁用
	out1, err1 := runCmdOutput("powershell", "-NoProfile", "-Command",
		fmt.Sprintf("(Get-NetIPInterface -InterfaceAlias '%s' -ErrorAction SilentlyContinue).AutomaticMetric", name))
	if err1 == nil {
		out1 = strings.TrimSpace(strings.ToLower(out1))
		if out1 == "disabled" || out1 == "0" {
			// 同时检查 metric 是否为 1
			out2, err2 := runCmdOutput("powershell", "-NoProfile", "-Command",
				fmt.Sprintf("(Get-NetIPInterface -InterfaceAlias '%s' -ErrorAction SilentlyContinue).InterfaceMetric", name))
			if err2 == nil && strings.TrimSpace(out2) == "1" {
				return true
			}
		}
	}
	return false
}

// disableAllPhysicalAutoMetric 枚举所有活跃物理网卡并禁用其 AutomaticMetric。
func disableAllPhysicalAutoMetric(tunName string) {
	var bufLen uint32 = 15000
	buf := make([]byte, bufLen)

	r1, _, _ := procGetAdaptersAddresses.Call(
		uintptr(syscall.AF_INET),
		uintptr(gaaFlagSkipUnicast|gaaFlagSkipAnycast|gaaFlagSkipMulticast|gaaFlagSkipDNS),
		0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&bufLen)),
	)
	if r1 != 0 {
		log.Printf("[tun] enumerate NICs: GetAdaptersAddresses ret=%d", r1)
		return
	}

	p := (*ipAdapterAddresses)(unsafe.Pointer(&buf[0]))
	for p != nil {
		nicName := windows.UTF16PtrToString(p.FriendlyName)
		// 跳过 TUN、未启用、回环 (IF_TYPE_SOFTWARE_LOOPBACK = 24)
		if nicName != tunName && p.OperStatus == 1 && p.IfType != 24 {
			if err := setMetricAPI(p.IfIndex); err != nil {
				log.Printf("[tun] disable AutoMetric %q: %v", nicName, err)
			} else {
				log.Printf("[tun] disabled AutoMetric: %s (idx=%d)", nicName, p.IfIndex)
			}
		}
		p = p.Next
	}
}
