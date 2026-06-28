package netutil

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

// TCPTransport provides a TCP fallback transport for when UDP is blocked.
// It wraps a TCP connection with length-prefixed framing to carry the same
// protocol messages as the UDP transport.
//
// Wire format over TCP: [2 bytes: length (little-endian)] [length bytes: packet]
// This avoids TCP's stream-based issues (partial reads, coalescing).
type TCPTransport struct {
	conn    net.Conn
	mu      sync.Mutex // protects writes (TCP is not safe for concurrent writes)
	closed  bool
	closeCh chan struct{}
}

// NewTCPTransport wraps an existing TCP connection.
func NewTCPTransport(conn net.Conn) *TCPTransport {
	return &TCPTransport{
		conn:    conn,
		closeCh: make(chan struct{}),
	}
}

// DialTCP connects to the server via TCP.
// addr should be "host:port" (typically the same as the UDP server address).
func DialTCP(addr string, timeout time.Duration) (*TCPTransport, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("tcp dial: %w", err)
	}
	// Set TCP_NODELAY for low latency (disable Nagle's algorithm)
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	return NewTCPTransport(conn), nil
}

// Send writes a protocol packet to the TCP connection with length-prefix framing.
// Thread-safe (uses mutex for concurrent writes from multiple goroutines).
func (t *TCPTransport) Send(data []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return fmt.Errorf("tcp transport closed")
	}

	// Length-prefix: 2 bytes little-endian
	if len(data) > 65535 {
		return fmt.Errorf("packet too large for TCP framing: %d bytes", len(data))
	}

	var header [2]byte
	binary.LittleEndian.PutUint16(header[:], uint16(len(data)))

	if _, err := t.conn.Write(header[:]); err != nil {
		return fmt.Errorf("tcp write header: %w", err)
	}
	if _, err := t.conn.Write(data); err != nil {
		return fmt.Errorf("tcp write data: %w", err)
	}
	return nil
}

// Receive reads one protocol packet from the TCP connection.
// Blocks until a complete packet is available or an error occurs.
// Returns the raw protocol packet (without length prefix).
func (t *TCPTransport) Receive() ([]byte, error) {
	// Read 2-byte length header
	var header [2]byte
	if _, err := io.ReadFull(t.conn, header[:]); err != nil {
		return nil, fmt.Errorf("tcp read header: %w", err)
	}

	length := int(binary.LittleEndian.Uint16(header[:]))
	if length == 0 || length > 65535 {
		return nil, fmt.Errorf("invalid TCP frame length: %d", length)
	}

	// Read the full packet
	buf := make([]byte, length)
	if _, err := io.ReadFull(t.conn, buf); err != nil {
		return nil, fmt.Errorf("tcp read data: %w", err)
	}

	return buf, nil
}

// Close closes the TCP connection.
func (t *TCPTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return nil
	}
	t.closed = true
	close(t.closeCh)
	return t.conn.Close()
}

// RemoteAddr returns the remote address of the TCP connection.
func (t *TCPTransport) RemoteAddr() net.Addr {
	return t.conn.RemoteAddr()
}

// TCPListener listens for incoming TCP connections and creates transports.
type TCPListener struct {
	listener net.Listener
}

// NewTCPListener creates a TCP listener on the given address.
func NewTCPListener(addr string) (*TCPListener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("tcp listen: %w", err)
	}
	return &TCPListener{listener: ln}, nil
}

// Accept waits for and returns the next TCP connection as a transport.
func (tl *TCPListener) Accept() (*TCPTransport, error) {
	conn, err := tl.listener.Accept()
	if err != nil {
		return nil, err
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	return NewTCPTransport(conn), nil
}

// Addr returns the listener's address.
func (tl *TCPListener) Addr() net.Addr {
	return tl.listener.Addr()
}

// Close stops listening.
func (tl *TCPListener) Close() error {
	return tl.listener.Close()
}

// UDPTCPBridge bridges a TCP transport with a UDP socket, allowing TCP clients
// to communicate with the existing UDP-based server protocol.
//
// Usage: When a client connects via TCP (because UDP is blocked), the server
// creates a bridge that:
// 1. Reads packets from TCP and injects them into the UDP processing pipeline
// 2. Writes outbound packets (destined for this client) back to TCP
type UDPTCPBridge struct {
	tcp        *TCPTransport
	virtualIP  net.IP        // the client's assigned virtual IP
	remoteAddr *net.UDPAddr  // synthetic address for protocol compatibility
	done       chan struct{}
}

// NewUDPTCPBridge creates a bridge between a TCP transport and the server's
// UDP processing pipeline. remoteAddr is a synthetic UDP address used to
// identify this client in the server's address maps.
func NewUDPTCPBridge(tcp *TCPTransport, syntheticAddr *net.UDPAddr) *UDPTCPBridge {
	return &UDPTCPBridge{
		tcp:        tcp,
		remoteAddr: syntheticAddr,
		done:       make(chan struct{}),
	}
}

// RemoteAddr returns the synthetic UDP address for this TCP client.
func (b *UDPTCPBridge) RemoteAddr() *net.UDPAddr {
	return b.remoteAddr
}

// ReceiveLoop reads packets from TCP and calls the handler for each one.
// Runs until the TCP connection is closed or the bridge is stopped.
func (b *UDPTCPBridge) ReceiveLoop(handler func(data []byte, addr *net.UDPAddr)) {
	defer close(b.done)
	for {
		data, err := b.tcp.Receive()
		if err != nil {
			log.Printf("[tcp-bridge] receive error: %v", err)
			return
		}
		handler(data, b.remoteAddr)
	}
}

// Send writes a protocol packet to the TCP connection.
func (b *UDPTCPBridge) Send(data []byte) error {
	return b.tcp.Send(data)
}

// Stop signals the bridge to stop.
func (b *UDPTCPBridge) Stop() {
	b.tcp.Close()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-b.done:
	case <-timer.C:
	}
}
