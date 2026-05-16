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

	procGetAdaptersAddresses = modIphlpapi.NewProc("GetAdaptersAddresses")
	procGetIpInterfaceEntry  = modIphlpapi.NewProc("GetIpInterfaceEntry")
	procSetIpInterfaceEntry  = modIphlpapi.NewProc("SetIpInterfaceEntry")
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

// setMetricAPI 通过 IP Helper API 禁用指定网卡的 AutomaticMetric。
//
// 使用原始字节缓冲区代替 Go 结构体，确保字段偏移量与 Windows SDK 完全一致。
func setMetricAPI(ifIndex uint32, luid uint64) error {
	// 分配足够大的缓冲区
	row := make([]byte, mibRowSize)

	// 设置 Family = AF_INET (2)
	binary.LittleEndian.PutUint16(row[offsetFamily:], syscall.AF_INET)

	// 设置 InterfaceLuid
	binary.LittleEndian.PutUint64(row[offsetInterfaceLuid:], luid)

	// 设置 InterfaceIndex
	binary.LittleEndian.PutUint32(row[offsetInterfaceIndex:], ifIndex)

	// GetIpInterfaceEntry 填充整行
	r1, _, e1 := procGetIpInterfaceEntry.Call(uintptr(unsafe.Pointer(&row[0])))
	if r1 != 0 {
		return fmt.Errorf("GetIpInterfaceEntry(idx=%d): ret=%d err=%v", ifIndex, r1, e1)
	}

	// 修改 UseAutomaticMetric = 0 (禁用自动 metric)
	binary.LittleEndian.PutUint32(row[offsetUseAutoMetric:], 0)

	// SetIpInterfaceEntry 写回
	r1, _, e1 = procSetIpInterfaceEntry.Call(uintptr(unsafe.Pointer(&row[0])))
	if r1 != 0 {
		return fmt.Errorf("SetIpInterfaceEntry(idx=%d): ret=%d err=%v", ifIndex, r1, e1)
	}
	return nil
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
func checkAutoMetricDisabled(name string) bool {
	idx, luid, err := findAdapter(name)
	if err != nil {
		return false
	}
	row := make([]byte, mibRowSize)
	binary.LittleEndian.PutUint16(row[offsetFamily:], syscall.AF_INET)
	binary.LittleEndian.PutUint64(row[offsetInterfaceLuid:], luid)
	binary.LittleEndian.PutUint32(row[offsetInterfaceIndex:], idx)
	r1, _, _ := procGetIpInterfaceEntry.Call(uintptr(unsafe.Pointer(&row[0])))
	if r1 != 0 {
		return false
	}
	return binary.LittleEndian.Uint32(row[offsetUseAutoMetric:]) == 0
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
