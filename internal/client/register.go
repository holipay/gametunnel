package client

import (
	"context"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/holipay/gametunnel/internal/auth"
	"github.com/holipay/gametunnel/internal/crypto"
	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/netutil"
	"github.com/holipay/gametunnel/internal/protocol"
)

// register performs the registration handshake with the server.
// It handles both passwordless and HMAC challenge-response flows.
// conn is passed explicitly to avoid races with Connect() reassigning t.conn.
func (t *Tunnel) register(ctx context.Context, conn *net.UDPConn) error {
	reg := &protocol.RegisterPayload{
		RoomID:   t.session.roomID,
		Username: t.session.username,
		Version:  protocol.AppVersion,
	}
	packet := protocol.EncodeChecked(protocol.TypeRegister, reg.Marshal())

	deadline := 10 * time.Second
	if err := conn.SetReadDeadline(time.Now().Add(deadline)); err != nil {
		log.Printf("register set read deadline: %v", err)
	}
	defer func() {
		if err := conn.SetReadDeadline(time.Time{}); err != nil {
			log.Printf("register clear read deadline: %v", err)
		}
	}()

	const maxRetries = 3
	retries := 0
	authRounds := 0
	pendingPacket := packet

	t.writeUDP(conn, pendingPacket, t.serverAddr.Load())

	respBuf := make([]byte, 1500)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		msg, err := t.readResponse(ctx, conn, respBuf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				retries++
				if retries > maxRetries {
					return fmt.Errorf("%s", i18n.Format(i18n.T().LogRegFailed, maxRetries))
				}
				log.Printf("%s", i18n.Format(i18n.T().LogRegTimeout, retries, maxRetries))
				t.writeUDP(conn, pendingPacket, t.serverAddr.Load())
				conn.SetReadDeadline(time.Now().Add(deadline))
				continue
			}
			return err
		}

		retries = 0

		done, err := t.handleRegResponse(msg, conn, deadline, &authRounds)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		conn.SetReadDeadline(time.Now().Add(deadline))
	}
}

// registerTCP performs registration over TCP transport.
// Used when UDP is blocked (e.g. strict firewalls).
func (t *Tunnel) registerTCP(ctx context.Context) error {
	reg := &protocol.RegisterPayload{
		RoomID:   t.session.roomID,
		Username: t.session.username,
		Version:  protocol.AppVersion,
	}
	packet := protocol.EncodeChecked(protocol.TypeRegister, reg.Marshal())

	if err := t.tcpTransport.Send(packet); err != nil {
		return fmt.Errorf("tcp send register: %w", err)
	}

	deadline := 10 * time.Second
	// Clear read deadline when done (matches UDP register cleanup).
	defer t.tcpTransport.SetReadDeadline(time.Time{})

	const maxRetries = 3
	retries := 0
	authRounds := 0

	for {
		// Set a read deadline so Receive() can be interrupted by timeout.
		if err := t.tcpTransport.SetReadDeadline(time.Now().Add(deadline)); err != nil {
			return fmt.Errorf("tcp set read deadline: %w", err)
		}

		data, err := t.tcpTransport.Receive()
		if err != nil {
			// Check context cancellation first.
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			// Distinguish timeout from other errors.
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				retries++
				if retries > maxRetries {
					return fmt.Errorf("TCP registration timeout after %d retries", maxRetries)
				}
				log.Printf("TCP registration timeout, retrying (%d/%d)...", retries, maxRetries)
				continue
			}
			return fmt.Errorf("tcp receive: %w", err)
		}

		retries = 0

		msg, err := protocol.DecodeChecked(data)
		if err != nil {
			continue
		}

		done, err := t.handleRegResponse(msg, nil, deadline, &authRounds)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

// handleRegResponse processes a single registration response message.
// Returns done=true if the registration is complete (assigned IP or kicked).
// conn is nil for TCP transport.
func (t *Tunnel) handleRegResponse(msg *protocol.Message, conn *net.UDPConn, deadline time.Duration, authRounds *int) (done bool, err error) {
	const maxAuthRounds = 3
	switch msg.Type {
	case protocol.TypeAssignIP:
		return true, t.handleAssignIP(msg.Payload)
	case protocol.TypeAuthChallenge:
		*authRounds++
		if *authRounds > maxAuthRounds {
			return true, fmt.Errorf("%s", i18n.T().ErrTooManyAuth)
		}
		if err := t.handleAuthChallenge(conn, msg.Payload); err != nil {
			return false, err
		}
		return false, nil
	case protocol.TypeKick:
		kick, err := protocol.UnmarshalKick(msg.Payload)
		if err != nil || kick == nil {
			return true, fmt.Errorf("%s", i18n.T().ErrRejected)
		}
		return true, fmt.Errorf("%s", i18n.Format(i18n.T().ErrRejected, kick.Reason))
	}
	return false, nil
}

// readResponse reads and decodes one protocol message from the server.
func (t *Tunnel) readResponse(ctx context.Context, conn *net.UDPConn, buf []byte) (*protocol.Message, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		return nil, err
	}

	msg, err := protocol.DecodeChecked(buf[:n])
	if err != nil {
		return nil, fmt.Errorf("%s", i18n.Format(i18n.T().ErrDecodeFailed, err))
	}
	return msg, nil
}

// handleAssignIP processes the server's IP assignment.
func (t *Tunnel) handleAssignIP(payload []byte) error {
	assign, err := protocol.UnmarshalAssignIP(payload)
	if err != nil {
		return fmt.Errorf("%s", i18n.Format(i18n.T().ErrParseIPFailed, err))
	}

	// Version compatibility check
	if !protocol.IsCompatible(protocol.AppVersion, assign.Version) {
		return fmt.Errorf("server version v%d is incompatible with client v%d",
			assign.Version,
			protocol.AppVersion)
	}

	// Validate that assigned IPs are IPv4 and within the assigned subnet.
	subnet := &net.IPNet{
		IP:   assign.VirtualIP.Mask(net.IPMask(assign.SubnetMask)),
		Mask: net.IPMask(assign.SubnetMask),
	}
	if assign.VirtualIP.To4() == nil || !subnet.Contains(assign.VirtualIP) {
		return fmt.Errorf("assigned IP %s is not in subnet %s", assign.VirtualIP, subnet)
	}
	if assign.ServerIP.To4() == nil || !subnet.Contains(assign.ServerIP) {
		return fmt.Errorf("server IP %s is not in subnet %s", assign.ServerIP, subnet)
	}

	// Initialize end-to-end encryption if password is set
	var encCipher, decCipher, p2pCipher *crypto.Cipher
	if t.session.roomPass != "" {
		// Always use the password-derived key for client-to-client encryption.
		// ECDH session keys are unique per client-server pair, so they cannot be
		// used for relay or P2P paths where both endpoints are different clients.
		// The server relays encrypted payloads transparently without decrypting,
		// so the sender and receiver must share the same key.
		key := auth.DeriveKey(t.session.roomPass, t.session.roomID)
		if key == nil {
			return fmt.Errorf("%s", i18n.T().ErrDeriveKeyFailed)
		}
		if encCipher, err = crypto.NewCipher(key, crypto.DirClientToServer); err != nil {
			return fmt.Errorf("init encrypt cipher: %w", err)
		}
		if decCipher, err = crypto.NewCipher(key, crypto.DirServerToClient); err != nil {
			return fmt.Errorf("init decrypt cipher: %w", err)
		}
		if p2pCipher, err = crypto.NewCipher(key, crypto.DirClientToClient); err != nil {
			return fmt.Errorf("init p2p cipher: %w", err)
		}
		log.Printf("[tunnel] encryption enabled (ChaCha20-Poly1305)")
	}

	// Cache subnet and serverIPKey for hot-path lookups
	cachedSubnet := &net.IPNet{
		IP:   assign.VirtualIP.Mask(net.IPMask(assign.SubnetMask)),
		Mask: net.IPMask(assign.SubnetMask),
	}
	serverIPKey := netutil.IPKey(assign.ServerIP)

	// Cache the hole punch packet once — reused by startHolePunch,
	// handleHolePunchReceived, and sendP2PKeepalives.
	cachedPunchPacket := protocol.EncodeChecked(protocol.TypeHolePunch, assign.VirtualIP.To4())

	// Atomically update all fields under lock to prevent races with readers
	t.mu.Lock()
	t.session.virtualIP = assign.VirtualIP
	t.session.serverIP = assign.ServerIP
	t.session.subnetMask = net.IPMask(assign.SubnetMask)
	t.session.serverVersion.Store(uint32(assign.Version))
	t.session.sessionToken = assign.SessionToken
	t.session.cachedSubnet.Store(cachedSubnet)
	t.session.serverIPKey.Store(&serverIPKey)
	t.nat.cachedPunchPacket.Store(cachedPunchPacket)
	t.crypto.encCipher = encCipher
	t.crypto.decCipher = decCipher
	t.crypto.decAvailable.Store(decCipher != nil)
	t.crypto.p2pCipher = p2pCipher
	// Clear stale peers from previous session — they will be repopulated
	// by the next PeerInfo message from the server.
	t.peers = make(map[[16]byte]*Peer)
	t.peerSnapshot.Store(t.peers)
	t.mu.Unlock()

	return nil
}

// handleAuthChallenge responds to the server's HMAC authentication challenge.
func (t *Tunnel) handleAuthChallenge(conn *net.UDPConn, payload []byte) error {
	if t.session.roomPass == "" {
		return fmt.Errorf("%s", i18n.T().ErrNeedPassword)
	}

	acp, err := protocol.UnmarshalAuthChallenge(payload)
	if err != nil {
		return fmt.Errorf("%s", i18n.Format(i18n.T().ErrParseAuthFailed, err))
	}

	key := auth.DeriveKey(t.session.roomPass, t.session.roomID)
	if key == nil {
		return fmt.Errorf("%s", i18n.T().ErrDeriveKeyFailed)
	}

	// 使用服务端观测到的客户端地址（经过 NAT 后的公网地址）
	var clientAddr *net.UDPAddr
	if acp.ClientAddr != "" {
		var err error
		clientAddr, err = net.ResolveUDPAddr("udp", acp.ClientAddr)
		if err != nil {
			log.Printf("resolve client addr: %v", err)
		}
	}

	hmacVal := auth.ComputeHMAC(key, acp.Challenge, t.session.roomID, t.session.username, clientAddr)

	resp := &protocol.AuthResponsePayload{
		RoomID:   t.session.roomID,
		Username: t.session.username,
		HMAC:     hmacVal,
	}

	packet := protocol.EncodeChecked(protocol.TypeAuthResponse, resp.Marshal())
	if conn != nil {
		t.writeUDP(conn, packet, t.serverAddr.Load())
	} else if t.tcpTransport != nil {
		if err := t.tcpTransport.Send(packet); err != nil {
			return fmt.Errorf("tcp send auth: %w", err)
		}
	}

	log.Printf("%s", i18n.T().LogAuthSent)
	return nil
}
