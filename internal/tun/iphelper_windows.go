//go:build windows

// iphelper_windows.go — IP Helper API bindings for route and IP address management.
//
// Replaces shell commands (route add/delete, netsh, PowerShell) with direct
// Win32 API calls for reliability and performance.

package tun

import (
	"fmt"
	"net"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procCreateIpForwardEntry2 = modIphlpapi.NewProc("CreateIpForwardEntry2")
	procDeleteIpForwardEntry2 = modIphlpapi.NewProc("DeleteIpForwardEntry2")
	procGetIpForwardTable2    = modIphlpapi.NewProc("GetIpForwardTable2")
	procSetIpInterfaceEntry            = modIphlpapi.NewProc("SetIpInterfaceEntry")
	procGetIpInterfaceEntry            = modIphlpapi.NewProc("GetIpInterfaceEntry")
	procCreateUnicastIpAddressEntry    = modIphlpapi.NewProc("CreateUnicastIpAddressEntry")
	procDeleteUnicastIpAddressEntry    = modIphlpapi.NewProc("DeleteUnicastIpAddressEntry")
)

// ipToSockaddrInet converts a net.IP to a SOCKADDR_INET for use with IP Helper APIs.
//
// Layout of SOCKADDR_INET (union):
//
//	AF_INET:  Family(2) + Port(2) + sin_addr(4) + zero(8) = 16 bytes
//	AF_INET6: Family(2) + Port(2) + Flowinfo(4) + Addr(16) + ScopeId(4) = 28 bytes
//
// RawSockaddrInet.Data is [6]uint32 starting at offset 4.
// For AF_INET, sin_addr overlaps Data[0].
// For AF_INET6, sin6_addr overlaps Data[1..4], Flowinfo overlaps Data[0].
func ipToSockaddrInet(ip net.IP) windows.RawSockaddrInet {
	var sa windows.RawSockaddrInet
	ip4 := ip.To4()
	if ip4 != nil {
		sa.Family = windows.AF_INET
		sa.Data[0] = uint32(ip4[0]) | uint32(ip4[1])<<8 | uint32(ip4[2])<<16 | uint32(ip4[3])<<24
	} else {
		ip16 := ip.To16()
		sa.Family = windows.AF_INET6
		// Data[0] = Flowinfo (leave as 0)
		// Data[1..4] = Addr (16 bytes)
		for i := 0; i < 4; i++ {
			sa.Data[1+i] = uint32(ip16[i*4]) | uint32(ip16[i*4+1])<<8 | uint32(ip16[i*4+2])<<16 | uint32(ip16[i*4+3])<<24
		}
	}
	return sa
}

// addRoute adds a route to the system routing table via CreateIpForwardEntry2.
//
// InitializeIpForwardEntry is skipped — it only zeros the struct (which Go's
// var declaration already does) and sets defaults we don't need. On some Windows
// systems it returns ERROR_GEN_FAILURE (0xFFFFFFFF) for no clear reason, breaking
// all route operations. Skipping it has been tested to work reliably.
func addRoute(luid uint64, dest net.IP, mask net.IPMask, nextHop net.IP, metric uint32) error {
	var row windows.MibIpForwardRow2

	prefixLen, _ := mask.Size()

	row.InterfaceLuid = luid
	row.DestinationPrefix = windows.IpAddressPrefix{
		Prefix:       ipToSockaddrInet(dest),
		PrefixLength: uint8(prefixLen),
	}
	row.NextHop = ipToSockaddrInet(nextHop)
	row.Metric = metric
	row.Protocol = windows.MIB_IPPROTO_NT_STATIC

	r1, _, _ := procCreateIpForwardEntry2.Call(uintptr(unsafe.Pointer(&row)))
	if r1 != 0 {
		return fmt.Errorf("CreateIpForwardEntry2(%s/%d via %s metric=%d): ret=%d",
			dest, prefixLen, nextHop, metric, r1)
	}
	return nil
}

// deleteRoute removes a route from the system routing table via DeleteIpForwardEntry2.
func deleteRoute(luid uint64, dest net.IP, mask net.IPMask, nextHop net.IP) error {
	var row windows.MibIpForwardRow2
	prefixLen, _ := mask.Size()

	row.InterfaceLuid = luid
	row.DestinationPrefix = windows.IpAddressPrefix{
		Prefix:       ipToSockaddrInet(dest),
		PrefixLength: uint8(prefixLen),
	}
	if nextHop != nil {
		row.NextHop = ipToSockaddrInet(nextHop)
	}

	r1, _, _ := procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(&row)))
	if r1 != 0 {
		return fmt.Errorf("DeleteIpForwardEntry2(%s/%d): ret=%d", dest, prefixLen, r1)
	}
	return nil
}

// ipToSockaddrInet4ForRow creates a RawSockaddrInet6 suitable for MibUnicastIpAddressRow.Address.
//
// SOCKADDR_INET union layout:
//
//	AF_INET (SOCKADDR_IN):  Family(2) + Port(2) + sin_addr(4) + zero(8) = 16 bytes
//	AF_INET6:               Family(2) + Port(2) + Flowinfo(4) + Addr(16) + ScopeId(4) = 28 bytes
//
// For AF_INET, sin_addr is at offset 4, which overlaps with Flowinfo in the
// RawSockaddrInet6 layout. So the IPv4 address must be written into Flowinfo.
func ipToSockaddrInet4ForRow(ip net.IP) windows.RawSockaddrInet6 {
	var sa windows.RawSockaddrInet6
	sa.Family = windows.AF_INET
	ip4 := ip.To4()
	if ip4 != nil {
		sa.Flowinfo = uint32(ip4[0]) | uint32(ip4[1])<<8 |
			uint32(ip4[2])<<16 | uint32(ip4[3])<<24
	}
	return sa
}

// addIPAddress assigns a unicast IP address to an interface.
func addIPAddress(luid uint64, ifIndex uint32, ip net.IP, mask net.IPMask) error {
	prefixLen, _ := mask.Size()

	var row windows.MibUnicastIpAddressRow
	row.Address = ipToSockaddrInet4ForRow(ip)
	row.InterfaceLuid = luid
	row.InterfaceIndex = ifIndex
	row.OnLinkPrefixLength = uint8(prefixLen)
	row.ValidLifetime = 0xFFFFFFFF   // INFINITE_LIFETIME
	row.PreferredLifetime = 0xFFFFFFFF

	r1, _, _ := procCreateUnicastIpAddressEntry.Call(uintptr(unsafe.Pointer(&row)))
	if r1 != 0 {
		return fmt.Errorf("CreateUnicastIpAddressEntry(%s/%d): ret=%d", ip, prefixLen, r1)
	}
	return nil
}

// deleteIPAddress removes a unicast IP address from an interface.
func deleteIPAddress(luid uint64, ifIndex uint32, ip net.IP) error {
	var row windows.MibUnicastIpAddressRow
	row.Address = ipToSockaddrInet4ForRow(ip)
	row.InterfaceLuid = luid
	row.InterfaceIndex = ifIndex

	r1, _, _ := procDeleteUnicastIpAddressEntry.Call(uintptr(unsafe.Pointer(&row)))
	if r1 != 0 {
		return fmt.Errorf("DeleteUnicastIpAddressEntry(%s): ret=%d", ip, r1)
	}
	return nil
}

// setInterfaceMetric sets the metric and disables AutomaticMetric on an interface.
func setInterfaceMetric(luid uint64, family uint16, metric uint32) error {
	var row windows.MibIpInterfaceRow
	row.Family = family
	row.InterfaceLuid = luid

	// Read existing row first (SetIpInterfaceEntry requires this on some Windows versions)
	r1, _, _ := procGetIpInterfaceEntry.Call(uintptr(unsafe.Pointer(&row)))
	if r1 != 0 {
		return fmt.Errorf("GetIpInterfaceEntry(luid=%d): ret=%d", luid, r1)
	}

	row.UseAutomaticMetric = 0
	row.Metric = metric

	r1, _, _ = procSetIpInterfaceEntry.Call(uintptr(unsafe.Pointer(&row)))
	if r1 != 0 {
		return fmt.Errorf("SetIpInterfaceEntry(luid=%d metric=%d): ret=%d", luid, metric, r1)
	}
	return nil
}

// detectPhysicalGateway finds the default gateway on a physical NIC (non-TUN).
// prefix should be "0.0.0.0/0" for IPv4 or "::/0" for IPv6.
// Returns the gateway IP and the interface index, or nil and 0 if not found.
func detectPhysicalGateway(prefix string, tunName string) (gateway net.IP, ifIndex uint32, err error) {
	var table *windows.MibIpForwardTable2
	r1, _, _ := procGetIpForwardTable2.Call(
		uintptr(syscall.AF_UNSPEC),
		uintptr(unsafe.Pointer(&table)),
		0, // allocate
	)
	defer func() {
		if table != nil {
			windows.FreeMibTable(unsafe.Pointer(table))
		}
	}()
	if r1 != 0 {
		return nil, 0, fmt.Errorf("GetIpForwardTable2: ret=%d", r1)
	}

	wantIPv4 := prefix == "0.0.0.0/0"
	wantFamily := uint16(windows.AF_INET6)
	if wantIPv4 {
		wantFamily = windows.AF_INET
	}

	for _, row := range table.Rows() {
		// Match address family
		family := row.DestinationPrefix.Prefix.Family
		if family != wantFamily {
			continue
		}

		// Check if it's a default route (prefix length 0)
		if row.DestinationPrefix.PrefixLength != 0 {
			continue
		}

		// Skip TUN interface
		if row.InterfaceIndex != 0 {
			name, err := findAdapterNameByIndex(row.InterfaceIndex)
			if err == nil && name == tunName {
				continue
			}
		}

		// Extract next-hop IP
		gw := sockaddrInetToIP(row.NextHop)
		if gw == nil || gw.IsUnspecified() {
			continue
		}
		return gw, row.InterfaceIndex, nil
	}

	return nil, 0, nil
}

// sockaddrInetToIP converts a SOCKADDR_INET to net.IP.
func sockaddrInetToIP(sa windows.RawSockaddrInet) net.IP {
	if sa.Family == windows.AF_INET {
		// SOCKADDR_IN: sin_addr at offset 4 = Data[0]
		addr := (*[4]byte)(unsafe.Pointer(&sa.Data[0]))
		return net.IPv4(addr[0], addr[1], addr[2], addr[3])
	}
	if sa.Family == windows.AF_INET6 {
		// SOCKADDR_INET6: sin6_addr at offset 8 = Data[1]
		// (Data[0] is Flowinfo, Data[1..4] is Addr)
		addr := (*[16]byte)(unsafe.Pointer(&sa.Data[1]))
		ip := make(net.IP, 16)
		copy(ip, addr[:])
		return ip
	}
	return nil
}
