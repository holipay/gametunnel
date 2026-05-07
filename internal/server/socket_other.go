//go:build !linux

package server

import "net"

func setSocketBuffers(_ *net.UDPConn) error {
	return nil
}
