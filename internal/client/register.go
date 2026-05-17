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
	}
	packet := protocol.EncodeChecked(protocol.TypeRegister, reg.Marshal())

	deadline := 10 * time.Second
	t.conn.SetReadDeadline(time.Now().Add(deadline))
	defer t.conn.SetReadDeadline(time.Time{})

	const maxRetries = 3
	const maxAuthRounds = 3
	retries := 0
	authRounds := 0

	t.writeUDP(packet, t.serverAddr)

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
				t.writeUDP(packet, t.serverAddr)
				t.conn.SetReadDeadline(time.Now().Add(deadline))
				continue
			}
			return err
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
			t.conn.SetReadDeadline(time.Now().Add(deadline))
			continue
		case protocol.TypeKick:
			kick, _ := protocol.UnmarshalKick(msg.Payload)
			return fmt.Errorf("%s", i18n.Format(i18n.T().ErrRejected, kick.Reason))
		}
	}
}

// readResponse reads and decodes one protocol message from the server.
// Caller must provide a reusable buffer (typically 1500 bytes).
func (t *Tunnel) readResponse(ctx context.Context, buf []byte) (*protocol.Message, error) {
	for {
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
}

// handleAssignIP processes the server's IP assignment.
func (t *Tunnel) handleAssignIP(payload []byte) error {
	assign, err := protocol.UnmarshalAssignIP(payload)
	if err != nil {
		return fmt.Errorf("%s", i18n.Format(i18n.T().ErrParseIPFailed, err))
	}
	t.virtualIP = assign.VirtualIP
	t.serverIP = assign.ServerIP
	t.subnetMask = net.IPMask(assign.SubnetMask)
	// Cache subnet and serverIPKey for hot-path lookups
	t.cachedSubnet = &net.IPNet{
		IP:   t.virtualIP.Mask(t.subnetMask),
		Mask: t.subnetMask,
	}
	t.serverIPKey = ipKey(t.serverIP)

	// Initialize end-to-end encryption if password is set
	if t.roomPass != "" {
		key := auth.DeriveKey(t.roomPass, t.roomID)
		if key != nil {
			if t.encCipher, err = crypto.NewCipher(key, crypto.DirClientToServer); err != nil {
				return fmt.Errorf("init encrypt cipher: %w", err)
			}
			if t.decCipher, err = crypto.NewCipher(key, crypto.DirServerToClient); err != nil {
				return fmt.Errorf("init decrypt cipher: %w", err)
			}
			log.Printf("[tunnel] encryption enabled (ChaCha20-Poly1305)")
		}
	}

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
		clientAddr, _ = net.ResolveUDPAddr("udp", acp.ClientAddr)
	}

	hmacVal := auth.ComputeHMAC(key, acp.Challenge, t.roomID, t.username, clientAddr)

	resp := &protocol.AuthResponsePayload{
		RoomID:   t.roomID,
		Username: t.username,
		HMAC:     hmacVal,
	}

	packet := protocol.EncodeChecked(protocol.TypeAuthResponse, resp.Marshal())
	t.writeUDP(packet, t.serverAddr)

	log.Printf("%s", i18n.T().LogAuthSent)
	return nil
}
