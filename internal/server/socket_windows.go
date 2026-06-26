//go:build windows

package server

import (
	"net"

	"github.com/holipay/gametunnel/internal/netutil"
)

func setSocketBuffers(conn *net.UDPConn) error {
	return netutil.SetSocketBuffers(conn)
}
