//go:build !linux && !windows

package netutil

import "net"

// SetSocketBuffers is a no-op on non-Linux/non-Windows platforms.
// OS defaults are generally sufficient.
func SetSocketBuffers(conn *net.UDPConn) error {
	return nil
}
