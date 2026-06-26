//go:build linux

package client

import (
	"net"
	"syscall"
)

const (
	// clientRcvBufSize is the client UDP receive buffer size (4MB).
	clientRcvBufSize = 4 * 1024 * 1024

	// clientSndBufSize is the client UDP send buffer size (2MB).
	clientSndBufSize = 2 * 1024 * 1024
)

func setClientSocketBuffers(conn *net.UDPConn) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return
	}
	raw.Control(func(fd uintptr) {
		syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, clientRcvBufSize)
		syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, clientSndBufSize)
	})
}
