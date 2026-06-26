//go:build !linux && !windows

package client

import "net"

func setClientSocketBuffers(conn *net.UDPConn) {
	// No-op on non-Linux/non-Windows platforms.
}
