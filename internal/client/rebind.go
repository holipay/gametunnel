package client

import (
	"net"
	"time"

	"github.com/holipay/gametunnel/internal/auth"
	"github.com/holipay/gametunnel/internal/protocol"
)

// tryRebind attempts to migrate the connection to the current network.
// Called when the server appears unreachable (keepalive timeout).
// Returns true if the server acknowledged the rebind.
//
// Flow:
//  1. Build a Rebind request with our virtual IP + HMAC (if password)
//  2. Send it to the server address (which may resolve via new network)
//  3. Wait for RebindAck via rebindAckCh (delivered by receiveFromServer)
//  4. If ack received, the server has updated our address — connection survived
func (t *Tunnel) tryRebind(timeout time.Duration) bool {
	t.mu.RLock()
	vip := t.virtualIP
	roomPass := t.roomPass
	roomID := t.roomID
	username := t.username
	t.mu.RUnlock()

	if vip == nil {
		return false
	}

	// Build rebind payload
	rebind := &protocol.RebindPayload{VirtualIP: vip}

	// If room has password, compute HMAC to prove ownership.
	// Bind to the virtual IP to prevent session hijacking via HMAC replay.
	if roomPass != "" {
		key := auth.DeriveKey(roomPass, roomID)
		if key == nil {
			return false
		}
		virtualAddr := &net.UDPAddr{IP: vip, Port: 0}
		rebind.HMAC = auth.ComputeHMAC(key, nil, roomID, username, virtualAddr)
	}

	packet := protocol.EncodeChecked(protocol.TypeRebind, rebind.Marshal())

	// Send rebind request (use ctrl channel for reliability)
	t.sendCtrl(packet, t.serverAddr.Load())

	// Drain any stale acks from previous attempts
	select {
	case <-t.rebindAckCh:
	default:
	}

	// Wait for ack via channel (no direct ReadFromUDP — avoids concurrent
	// reads with receiveFromServer)
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case ack := <-t.rebindAckCh:
		if ack.Success {
			t.markServerResponse() // reset timeout
			return true
		}
		return false
	case <-timer.C:
		return false
	}
}
