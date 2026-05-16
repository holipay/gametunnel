//go:build windows

// metric_windows.go — 使用 Windows IP Helper API 直接管理网卡 metric。
//
// 改用原始字节缓冲区操作 MIB_IPINTERFACE_ROW，避免 Go 结构体
// 与 Windows SDK 结构体因字段对齐、版本差异导致的布局不匹配。

package tun

import (
	"encoding/binary"
	"fmt"
	"log"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modIphlpapi = syscall.NewLazyDLL("iphlpapi.dll")

	procGetAdaptersAddresses       = modIphlpapi.NewProc("GetAdaptersAddresses")
	procGetIpInterfaceEntry        = modIphlpapi.NewProc("GetIpInterfaceEntry")
	procSetIpInterfaceEntry        = modIphlpapi.NewProc("SetIpInterfaceEntry")
)

const (
	gaaFlagSkipUnicast      = 0x0001
	gaaFlagSkipAnycast      = 0x0002
	gaaFlagSkipMulticast    = 0x0004
	gaaFlagSkipDNS          = 0x0008
	gaaFlagSkipFriendlyName = 0x0020
)

// MIB_IPINTERFACE_ROW 相关偏移量（Windows 10+ 通用）。
//
// 来源: Windows SDK 10.0.26100.0 netioapi.h
// 这些偏移量在 Windows 10/11 所有版本中保持稳定。
const (
	mibRowSize            = 416  // sizeof(MIB_IPINTERFACE_ROW) — Windows 10 1709+
	offsetFamily          = 0    // ADDRESS_FAMILY (uint16)
	offsetInterfaceLuid   = 8    // NET_LUID (uint64)
	offsetInterfaceIndex  = 16   // NET_IFINDEX (uint32)
	offsetUseAutoMetric   = 56   // ULONG UseAutomaticMetric
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

// setMetricAPI 通过 netsh 禁用指定网卡的 AutomaticMetric 并设置 metric 值。
//
// 直接调用 SetIpInterfaceEntry 对 wintun 虚拟适配器返回 ret=87，
// 因此改用 netsh 命令行方式，经测试可靠工作。
func setMetricAPI(ifIndex uint32, luid uint64) error {
	// 通过 ifIndex 找到接口名称
	name, err := findAdapterNameByIndex(ifIndex)
	if err != nil {
		return fmt.Errorf("find adapter name: %w", err)
	}

	// netsh interface ip set interface "name" metric=1
	if err := RunCmd("netsh", "interface", "ip", "set", "interface",
		fmt.Sprintf("name=%s", name), "metric=1"); err != nil {
		return fmt.Errorf("netsh set metric: %w", err)
	}

	log.Printf("[tun] AutomaticMetric disabled via netsh: %s (idx=%d)", name, ifIndex)
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
			// 用 raw buffer 读取 LUID
			row := make([]byte, mibRowSize)
			binary.LittleEndian.PutUint16(row[offsetFamily:], syscall.AF_INET)
			binary.LittleEndian.PutUint32(row[offsetInterfaceIndex:], p.IfIndex)
			r1, _, _ := procGetIpInterfaceEntry.Call(uintptr(unsafe.Pointer(&row[0])))
			if r1 == 0 {
				luid = binary.LittleEndian.Uint64(row[offsetInterfaceLuid:])
				return p.IfIndex, luid, nil
			}
			return 0, 0, fmt.Errorf("GetIpInterfaceEntry(idx=%d): adapter found but not ready", p.IfIndex)
		}
		p = p.Next
	}
	return 0, 0, fmt.Errorf("adapter %q not found", name)
}

// checkAutoMetricDisabled 检查网卡的 AutomaticMetric 是否已禁用。
// 通过 PowerShell 检查，因为 IP Helper API 的 GetIpInterfaceEntry
// 在 wintun 虚拟适配器上返回的 UseAutomaticMetric 字段不可靠。
func checkAutoMetricDisabled(name string) bool {
	out, err := runCmdOutput("powershell", "-NoProfile", "-Command",
		fmt.Sprintf("(Get-NetIPInterface -InterfaceAlias '%s' -ErrorAction SilentlyContinue).AutomaticMetric", name))
	if err != nil {
		return false
	}
	// 输出为空或 "Disabled" 表示已禁用
	out = strings.TrimSpace(strings.ToLower(out))
	return out == "" || out == "disabled" || out == "0"
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
			if err := setMetricAPI(p.IfIndex, 0); err != nil {
				log.Printf("[tun] disable AutoMetric %q: %v", nicName, err)
			} else {
				log.Printf("[tun] disabled AutoMetric: %s (idx=%d)", nicName, p.IfIndex)
			}
		}
		p = p.Next
	}
}
