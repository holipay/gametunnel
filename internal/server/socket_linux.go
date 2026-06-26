//go:build linux

package server

import (
	"net"
	"syscall"
)

const (
	// rcvBufSize is the UDP receive buffer size (8MB).
	// Default Linux value is ~212KB which can drop packets under burst traffic.
	rcvBufSize = 8 * 1024 * 1024

	// sndBufSize is the UDP send buffer size (4MB).
	sndBufSize = 4 * 1024 * 1024
)

func setSocketBuffers(conn *net.UDPConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var opErr error
	if err := raw.Control(func(fd uintptr) {
		opErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, rcvBufSize)
		if opErr != nil {
			return
		}
		opErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, sndBufSize)
	}); err != nil {
		return err
	}
	return opErr
}
