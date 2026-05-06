package client

import (
	"context"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/holipay/gametunnel/internal/auth"
	"github.com/holipay/gametunnel/internal/protocol"
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

	t.sendUDP(packet, t.serverAddr)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Wait for response (AssignIP, AuthChallenge, or Kick)
		msg, err := t.readResponse(ctx)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				retries++
				if retries > maxRetries {
					return fmt.Errorf("注册失败（重试%d次）", maxRetries)
				}
				log.Printf("[tunnel] 注册超时，重试 %d/%d...", retries, maxRetries)
				t.sendUDP(packet, t.serverAddr)
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
				return fmt.Errorf("认证失败：服务器发送了过多的认证请求")
			}
			if err := t.handleAuthChallenge(msg.Payload); err != nil {
				return err
			}
			t.conn.SetReadDeadline(time.Now().Add(deadline))
			continue
		case protocol.TypeKick:
			kick, _ := protocol.UnmarshalKick(msg.Payload)
			return fmt.Errorf("被拒绝: %s", kick.Reason)
		}
	}
}

// readResponse reads and decodes one protocol message from the server.
func (t *Tunnel) readResponse(ctx context.Context) (*protocol.Message, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		buf := make([]byte, 1500)
		n, _, err := t.conn.ReadFromUDP(buf)
		if err != nil {
			return nil, err
		}

		msg, err := protocol.DecodeChecked(buf[:n])
		if err != nil {
			return nil, fmt.Errorf("解码响应失败: %w", err)
		}
		return msg, nil
	}
}

// handleAssignIP processes the server's IP assignment.
func (t *Tunnel) handleAssignIP(payload []byte) error {
	assign, err := protocol.UnmarshalAssignIP(payload)
	if err != nil {
		return fmt.Errorf("解析IP分配失败: %w", err)
	}
	t.virtualIP = assign.VirtualIP
	t.serverIP = assign.ServerIP
	t.subnetMask = net.IPMask(assign.SubnetMask)
	return nil
}

// handleAuthChallenge responds to the server's HMAC authentication challenge.
func (t *Tunnel) handleAuthChallenge(payload []byte) error {
	if t.roomPass == "" {
		return fmt.Errorf("服务器需要房间密码，请用 -password 参数指定")
	}

	acp, err := protocol.UnmarshalAuthChallenge(payload)
	if err != nil {
		return fmt.Errorf("解析认证请求失败: %w", err)
	}

	key := auth.DeriveKey(t.roomPass, t.roomID)
	if key == nil {
		return fmt.Errorf("无法派生认证密钥")
	}

	hmacVal := auth.ComputeHMAC(key, acp.Challenge, t.roomID, t.username, t.serverAddr)

	resp := &protocol.AuthResponsePayload{
		RoomID:   t.roomID,
		Username: t.username,
		HMAC:     hmacVal,
	}

	packet := protocol.EncodeChecked(protocol.TypeAuthResponse, resp.Marshal())
	t.sendUDP(packet, t.serverAddr)

	log.Printf("[tunnel] 已发送认证响应，等待服务器确认...")
	return nil
}
