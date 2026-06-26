//go:build linux

package client

import (
	"net"

	"github.com/holipay/gametunnel/internal/netutil"
)

func setClientSocketBuffers(conn *net.UDPConn) {
	netutil.SetSocketBuffers(conn)
}
