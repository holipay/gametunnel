//go:build linux

package netutil

import (
	"net"
	"syscall"
)

const (
	// RcvBufSize is the UDP receive buffer size (8MB).
	RcvBufSize = 8 * 1024 * 1024

	// SndBufSize is the UDP send buffer size (4MB).
	SndBufSize = 4 * 1024 * 1024
)

// SetSocketBuffers tunes the UDP socket buffer sizes for high-throughput gaming.
func SetSocketBuffers(conn *net.UDPConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var opErr error
	if err := raw.Control(func(fd uintptr) {
		opErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, RcvBufSize)
		if opErr != nil {
			return
		}
		opErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, SndBufSize)
	}); err != nil {
		return err
	}
	return opErr
}
