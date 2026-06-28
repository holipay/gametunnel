package client

import (
	"context"
	"log"
	"time"

	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/protocol"
)

// keepaliveLoop sends periodic keepalive packets to the server and
// tracks the last time we received any data from the server (pong, peer info, etc.).
// If the server appears dead for too long, the tunnel context is cancelled
// to trigger a reconnect.
func (t *Tunnel) keepaliveLoop(ctx context.Context, cancel context.CancelFunc) {
	const serverTimeout = 30 * time.Second // 3 missed keepalives
	const rebindTimeout = 5 * time.Second  // how long to wait for rebind ack

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	packet := protocol.EncodeChecked(protocol.TypeKeepAlive, nil)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.sendCtrl(packet, t.serverAddr.Load())

			// Check if server is still alive
			lastSeen := t.lastServerResponse.Load()
			if lastSeen != 0 && time.Since(time.Unix(0, lastSeen)) > serverTimeout {
				// Server timeout — try connection migration before giving up
				if t.tryRebind(rebindTimeout) {
					log.Printf("[rebind] connection migrated successfully")
					continue // server responded, keep going
				}
				log.Printf(i18n.T().LogServerTimeout, serverTimeout)
				cancel()
				return
			}
		}
	}
}

// markServerResponse records that we received data from the server.
// Called from handleServerData to keep the liveness tracker updated.
func (t *Tunnel) markServerResponse() {
	t.lastServerResponse.Store(time.Now().UnixNano())
}
