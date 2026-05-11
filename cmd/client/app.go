package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/i18n"
)

// App wraps the tunnel with HTTP API and status tracking.
type App struct {
	cfg        *client.Config
	tunnel     *client.Tunnel
	mu         sync.RWMutex
	connecting bool
	lastErr    string

	// Status (updated by statusLoop polling tunnel.Status())
	connected  bool
	virtualIP  net.IP
	subnetMask net.IPMask
	serverIP   net.IP
	peerCount  int
	uptime     time.Time

	// TUN factory (set by platform-specific main)
	newTUN func(client.TunConfig) (client.TunDevice, error)

	// Context for connect loop
	ctx    context.Context
	cancel context.CancelFunc

	// onConnFailed is called when fast retries are exhausted.
	// Args: error message. Return true to retry, false to stop.
	onConnFailed func(errMsg string) bool
}

// PeerStatus is the JSON-serializable peer info.
type PeerStatus struct {
	Username  string `json:"username"`
	VirtualIP string `json:"virtual_ip"`
	RTT       string `json:"rtt"`
}

// StatusResponse is the SSE status payload.
type StatusResponse struct {
	Connected  bool   `json:"connected"`
	Connecting bool   `json:"connecting"`
	LastError  string `json:"last_error,omitempty"`
	VirtualIP  string `json:"virtual_ip,omitempty"`
	Subnet     string `json:"subnet,omitempty"`
	ServerIP   string `json:"server_ip,omitempty"`
	PeerCount  int    `json:"peer_count"`
	Uptime     string `json:"uptime,omitempty"`
	PlayerName string `json:"player_name"`
	RoomID     string `json:"room_id"`
	ServerAddr string `json:"server_addr"`
}

// NewApp creates a new App.
func NewApp(cfg *client.Config) *App {
	ctx, cancel := context.WithCancel(context.Background())
	return &App{
		cfg:    cfg,
		tunnel: client.New(cfg),
		ctx:    ctx,
		cancel: cancel,
	}
}

// SetTUNFactory sets the platform-specific TUN device factory.
func (a *App) SetTUNFactory(f func(client.TunConfig) (client.TunDevice, error)) {
	a.newTUN = f
}

// GetStatus returns the current status snapshot.
func (a *App) GetStatus() StatusResponse {
	a.mu.RLock()
	defer a.mu.RUnlock()

	s := StatusResponse{
		Connected:  a.connected,
		Connecting: a.connecting,
		LastError:  a.lastErr,
		PlayerName: a.cfg.PlayerName,
		RoomID:     a.cfg.RoomID,
		ServerAddr: a.cfg.ServerAddr,
		PeerCount:  a.peerCount,
	}

	if a.connected {
		s.VirtualIP = a.virtualIP.String()
		if a.subnetMask != nil {
			ones, _ := a.subnetMask.Size()
			s.Subnet = fmt.Sprintf("%s/%d", a.virtualIP.Mask(a.subnetMask), ones)
		}
		s.ServerIP = a.serverIP.String()
		if !a.uptime.IsZero() {
			s.Uptime = formatDuration(time.Since(a.uptime))
		}
	}

	return s
}

// Connect starts the tunnel connection in a goroutine.
func (a *App) Connect(cfg *client.Config) {
	a.mu.Lock()
	if a.connecting {
		a.mu.Unlock()
		return
	}
	a.connecting = true
	a.lastErr = ""
	a.mu.Unlock()

	// Clean up old tunnel before creating new one
	a.cancel()
	a.tunnel.Disconnect()
	a.tunnel.CloseTUN()

	// Update config and create new tunnel
	a.cfg = cfg
	a.tunnel = client.New(cfg)
	a.ctx, a.cancel = context.WithCancel(context.Background())

	if err := client.SaveConfig(cfg); err != nil {
		log.Printf(i18n.T().AppSaveFail, err)
	}

	go a.connectLoop()
}

// Disconnect stops the tunnel.
func (a *App) Disconnect() {
	a.cancel()
	a.tunnel.Disconnect()
	a.tunnel.CloseTUN()

	a.mu.Lock()
	a.connected = false
	a.connecting = false
	a.peerCount = 0
	a.mu.Unlock()

	// Reset context for next connection
	a.ctx, a.cancel = context.WithCancel(context.Background())
}

// statusLoop polls tunnel.Status() and syncs to App fields.
func (a *App) statusLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// Read tunnel pointer under lock to avoid race with Connect()
		a.mu.RLock()
		tun := a.tunnel
		a.mu.RUnlock()

		if tun == nil {
			continue
		}

		ts := tun.Status()

		a.mu.Lock()
		if ts.Connected {
			wasConnected := a.connected
			a.connected = true
			a.virtualIP = ts.VirtualIP
			a.subnetMask = ts.SubnetMask
			a.serverIP = ts.ServerIP
			a.peerCount = ts.PeerCount
			if !wasConnected {
				a.uptime = time.Now()
			}
		} else if a.connected {
			// Was connected, now disconnected
			a.connected = false
			a.peerCount = 0
		}
		a.mu.Unlock()
	}
}

// connectLoop handles connection with auto-reconnect.
// After fastRetries (3) rapid attempts, it pauses and calls onConnFailed
// to let the user decide: retry, edit settings, or stop.
func (a *App) connectLoop() {
	// Start status polling for this connection session
	pollCtx, pollCancel := context.WithCancel(a.ctx)
	defer pollCancel()
	go a.statusLoop(pollCtx)

	defer func() {
		a.mu.Lock()
		a.connecting = false
		a.mu.Unlock()
	}()

	const (
		baseDelay  = 2 * time.Second
		maxDelay   = 60 * time.Second
		fastRetries = 3 // number of rapid retries before prompting user
	)

	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			delay := baseDelay << (attempt - 1)
			if delay > maxDelay {
				delay = maxDelay
			}
			select {
			case <-a.ctx.Done():
				return
			case <-time.After(delay):
			}
		}

		err := a.tunnel.Connect(a.ctx, a.cfg.ServerAddr, 1400, a.newTUN)
		if a.ctx.Err() != nil {
			return
		}

		if err != nil {
			errMsg := err.Error()
			a.mu.Lock()
			a.lastErr = errMsg
			a.connected = false
			a.mu.Unlock()
			log.Printf(i18n.T().AppDisconnectErr, err)

			// After exhausting fast retries, prompt user instead of silent backoff
			if attempt+1 >= fastRetries {
				// Reset attempt counter so next round also gets fast retries
				if a.onConnFailed != nil {
					shouldRetry := a.onConnFailed(errMsg)
					if !shouldRetry {
						return
					}
					// User chose to retry — reset attempt for another round of fast retries
					attempt = -1
					continue
				}
			}
		} else {
			a.mu.Lock()
			a.connected = false
			a.lastErr = ""
			a.mu.Unlock()
			log.Printf(i18n.T().AppDisconnected)
			// Successful connection that later dropped — reset attempt counter
			attempt = -1
		}
	}
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// JSON serializes the status to JSON bytes.
func (s StatusResponse) JSON() []byte {
	b, _ := json.Marshal(s)
	return b
}
