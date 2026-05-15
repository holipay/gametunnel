//go:build !linux && !windows

package server

import "net"

func setSocketBuffers(_ *net.UDPConn) error {
	return nil
}
