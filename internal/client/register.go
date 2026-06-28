package client

import (
	"context"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/holipay/gametunnel/internal/auth"
	"github.com/holipay/gametunnel/internal/protocol"
	"github.com/holipay/gametunnel/internal/crypto"
	"github.com/holipay/gametunnel/internal/i18n"
)

// register performs the registration handshake with the server.
// It handles both passwordless and HMAC challenge-response flows.
func (t *Tunnel) register(ctx context.Context) error {
	reg := &protocol.RegisterPayload{
		RoomID:   t.roomID,
		Username: t.username,
		Version:  protocol.AppVersion,
	}
	packet := protocol.EncodeChecked(protocol.TypeRegister, reg.Marshal())

	deadline := 10 * time.Second
	t.conn.SetReadDeadline(time.Now().Add(deadline))
	defer t.conn.SetReadDeadline(time.Time{})

	const maxRetries = 3
	const maxAuthRounds = 3
	retries := 0
	authRounds := 0
	pendingPacket := packet

	t.writeUDP(t.conn, pendingPacket, t.serverAddr.Load())

	// Pre-allocate buffer for all readResponse calls during registration
	respBuf := make([]byte, 1500)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Wait for response (AssignIP, AuthChallenge, or Kick)
		msg, err := t.readResponse(ctx, respBuf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				retries++
				if retries > maxRetries {
					return fmt.Errorf("%s", i18n.Format(i18n.T().LogRegFailed, maxRetries))
				}
				log.Printf("%s", i18n.Format(i18n.T().LogRegTimeout, retries, maxRetries))
				t.writeUDP(t.conn, pendingPacket, t.serverAddr.Load())
				t.conn.SetReadDeadline(time.Now().Add(deadline))
				continue
			}
			return err
		}

		retries = 0

		switch msg.Type {
		case protocol.TypeAssignIP:
			return t.handleAssignIP(msg.Payload)
		case protocol.TypeAuthChallenge:
			authRounds++
			if authRounds > maxAuthRounds {
				return fmt.Errorf("%s", i18n.T().ErrTooManyAuth)
			}
			if err := t.handleAuthChallenge(msg.Payload); err != nil {
				return err
			}
			t.conn.SetReadDeadline(time.Now().Add(deadline))
			continue
		case protocol.TypeECDHExchange:
			if err := t.handleECDHExchange(msg.Payload); err != nil {
				return err
			}
			t.conn.SetReadDeadline(time.Now().Add(deadline))
			continue
		case protocol.TypeKick:
			kick, err := protocol.UnmarshalKick(msg.Payload)
			if err != nil || kick == nil {
				return fmt.Errorf("%s", i18n.T().ErrRejected)
			}
			return fmt.Errorf("%s", i18n.Format(i18n.T().ErrRejected, kick.Reason))
		}
	}
}

// readResponse reads and decodes one protocol message from the server.
// Caller must provide a reusable buffer (typically 1500 bytes).
func (t *Tunnel) readResponse(ctx context.Context, buf []byte) (*protocol.Message, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	n, _, err := t.conn.ReadFromUDP(buf)
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
	if t.roomPass != "" {
		// Use ECDH session key if negotiated, otherwise fall back to password-derived key
		var key []byte
		t.mu.RLock()
		if protocol.IsECDHNegotiated(assign.Version) && t.ecdhSessionKey != nil {
			key = t.ecdhSessionKey
		}
		t.mu.RUnlock()

		if key == nil {
			key = auth.DeriveKey(t.roomPass, t.roomID)
		}
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
		if protocol.IsECDHNegotiated(assign.Version) {
			log.Printf("[tunnel] encryption enabled (ChaCha20-Poly1305 + ECDH forward secrecy)")
		} else {
			log.Printf("[tunnel] encryption enabled (ChaCha20-Poly1305)")
		}
	}

	// Cache subnet and serverIPKey for hot-path lookups
	cachedSubnet := &net.IPNet{
		IP:   assign.VirtualIP.Mask(net.IPMask(assign.SubnetMask)),
		Mask: net.IPMask(assign.SubnetMask),
	}
	serverIPKey := ipKey(assign.ServerIP)

	// Cache the hole punch packet once — reused by startHolePunch,
	// handleHolePunchReceived, and sendP2PKeepalives.
	cachedPunchPacket := protocol.EncodeChecked(protocol.TypeHolePunch, assign.VirtualIP.To4())

	// Atomically update all fields under lock to prevent races with readers
	t.mu.Lock()
	t.virtualIP = assign.VirtualIP
	t.serverIP = assign.ServerIP
	t.subnetMask = net.IPMask(assign.SubnetMask)
	t.serverVersion = protocol.ClearECDHFlag(assign.Version)
	t.sessionToken = assign.SessionToken
	t.cachedSubnet = cachedSubnet
	t.serverIPKey = serverIPKey
	t.cachedPunchPacket = cachedPunchPacket
	t.encCipher = encCipher
	t.decCipher = decCipher
	t.p2pCipher = p2pCipher
	// Clear ECDH session key after use (prevent reuse)
	t.ecdhSessionKey = nil
	// Clear stale peers from previous session — they will be repopulated
	// by the next PeerInfo message from the server.
	t.peers = make(map[[16]byte]*Peer)
	t.mu.Unlock()

	return nil
}

// handleAuthChallenge responds to the server's HMAC authentication challenge.
func (t *Tunnel) handleAuthChallenge(payload []byte) error {
	if t.roomPass == "" {
		return fmt.Errorf("%s", i18n.T().ErrNeedPassword)
	}

	acp, err := protocol.UnmarshalAuthChallenge(payload)
	if err != nil {
		return fmt.Errorf("%s", i18n.Format(i18n.T().ErrParseAuthFailed, err))
	}

	key := auth.DeriveKey(t.roomPass, t.roomID)
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

	hmacVal := auth.ComputeHMAC(key, acp.Challenge, t.roomID, t.username, clientAddr)

	resp := &protocol.AuthResponsePayload{
		RoomID:   t.roomID,
		Username: t.username,
		HMAC:     hmacVal,
	}

	packet := protocol.EncodeChecked(protocol.TypeAuthResponse, resp.Marshal())
	t.writeUDP(t.conn, packet, t.serverAddr.Load())

	log.Printf("%s", i18n.T().LogAuthSent)
	return nil
}

// handleECDHExchange processes the server's ephemeral X25519 public key.
// Generates a client keypair, derives the shared secret, and sends ECDHConfirm.
func (t *Tunnel) handleECDHExchange(payload []byte) error {
	if t.roomPass == "" {
		return fmt.Errorf("ECDH exchange without password")
	}

	ecdhPkt, err := protocol.UnmarshalECDHExchange(payload)
	if err != nil {
		return fmt.Errorf("parse ECDH exchange: %w", err)
	}

	// Generate client's ephemeral keypair
	priv, clientPub, err := auth.GenerateECDHKeyPair()
	if err != nil {
		return fmt.Errorf("generate ECDH key: %w", err)
	}

	// Compute shared secret
	shared, err := auth.ComputeECDHSharedSecret(priv, ecdhPkt.PublicKey[:])
	if err != nil {
		return fmt.Errorf("ECDH shared secret: %w", err)
	}

	// Zero out private key material after shared secret derivation
	privBytes := priv.Bytes()
	for i := range privBytes {
		privBytes[i] = 0
	}

	// Derive session key
	sessionKey := auth.DeriveSessionKey(shared, t.roomID)

	// Zero out shared secret after key derivation
	for i := range shared {
		shared[i] = 0
	}
	if sessionKey == nil {
		return fmt.Errorf("derive session key failed")
	}

	// Compute HMAC over both public keys using password-derived key
	// (prevents MITM: attacker can't forge HMAC without password)
	passwordKey := auth.DeriveKey(t.roomPass, t.roomID)
	ecdhHMAC := auth.ComputeECDHMAC(passwordKey, ecdhPkt.PublicKey[:], clientPub)

	// Send ECDHConfirm
	confirm := &protocol.ECDHConfirmPayload{}
	copy(confirm.PublicKey[:], clientPub)
	copy(confirm.HMAC[:], ecdhHMAC)

	packet := protocol.EncodeChecked(protocol.TypeECDHConfirm, confirm.Marshal())
	t.writeUDP(t.conn, packet, t.serverAddr.Load())

	// Store session key for cipher creation in handleAssignIP
	t.mu.Lock()
	t.ecdhSessionKey = sessionKey
	t.mu.Unlock()

	log.Printf("[ecdh] session key negotiated")
	return nil
}

// registerTCP performs registration over TCP transport.
// Used when UDP is blocked (e.g. strict firewalls).
// The protocol is identical to UDP registration but uses TCP framing.
func (t *Tunnel) registerTCP(ctx context.Context) error {
	reg := &protocol.RegisterPayload{
		RoomID:   t.roomID,
		Username: t.username,
		Version:  protocol.AppVersion,
	}
	packet := protocol.EncodeChecked(protocol.TypeRegister, reg.Marshal())

	if err := t.tcpTransport.Send(packet); err != nil {
		return fmt.Errorf("tcp send register: %w", err)
	}

	// Read response via TCP
	deadline := 10 * time.Second
	timer := time.NewTimer(deadline)
	defer timer.Stop()

	const maxAuthRounds = 3
	authRounds := 0

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return fmt.Errorf("TCP registration timeout")
		default:
		}

		data, err := t.tcpTransport.Receive()
		if err != nil {
			return fmt.Errorf("tcp receive: %w", err)
		}

		msg, err := protocol.DecodeChecked(data)
		if err != nil {
			continue
		}

		switch msg.Type {
		case protocol.TypeAssignIP:
			return t.handleAssignIP(msg.Payload)
		case protocol.TypeAuthChallenge:
			authRounds++
			if authRounds > maxAuthRounds {
				return fmt.Errorf("%s", i18n.T().ErrTooManyAuth)
			}
			if err := t.handleAuthChallenge(msg.Payload); err != nil {
				return err
			}
			timer.Reset(deadline)
			continue
		case protocol.TypeECDHExchange:
			if err := t.handleECDHExchange(msg.Payload); err != nil {
				return err
			}
			timer.Reset(deadline)
			continue
		case protocol.TypeKick:
			kick, err := protocol.UnmarshalKick(msg.Payload)
			if err != nil || kick == nil {
				return fmt.Errorf("%s", i18n.T().ErrRejected)
			}
			return fmt.Errorf("%s", i18n.Format(i18n.T().ErrRejected, kick.Reason))
		}
	}
}
