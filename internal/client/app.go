// Package client provides the GameTunnel client tunnel and application logic.
// This file contains the App struct which wraps Tunnel with connection management,
// status tracking, and auto-reconnect. It is platform-independent.
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/protocol"
)

// App wraps the tunnel with connection management, status tracking, and auto-reconnect.
// Platform-specific code (tray, dialogs) accesses App through exported fields/methods.
type App struct {
	Cfg        *Config
	Tunnel     *Tunnel
	Mu         sync.RWMutex
	DialogMu   sync.Mutex // serializes platform modal dialogs (settings + error)
	Connecting bool
	ConnectGen int64 // generation counter to prevent stale defer from resetting Connecting
	LastErr    string

	// Status (updated by statusLoop polling tunnel.Status())
	Connected  bool
	VirtualIP  net.IP
	SubnetMask net.IPMask
	ServerIP   net.IP
	PeerCount  int
	Uptime     time.Time

	// TUN factory (set by platform-specific main)
	NewTUN func(TunConfig) (TunDevice, error)

	// Context for connect loop
	Ctx    context.Context
	Cancel context.CancelFunc

	// OnConnFailed is called when fast retries are exhausted.
	// Args: error message. Return true to retry, false to stop.
	OnConnFailed func(errMsg string) bool
}

// PeerStatus is the JSON-serializable peer info.
type PeerStatus struct {
	Username  string `json:"username"`
	VirtualIP string `json:"virtual_ip"`
	RTT       string `json:"rtt"`
}

// StatusResponse is the status payload for API/display.
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

	// Version info
	ServerVersion    string `json:"server_version,omitempty"`
	UpgradeAvailable bool   `json:"upgrade_available,omitempty"`
	UpgradeMessage   string `json:"upgrade_message,omitempty"`

	// Connection quality
	AvgRTT     float64 `json:"avg_rtt"`
	LossRate   float64 `json:"loss_rate"`
	P2PPeers   int     `json:"p2p_peers"`
	RelayPeers int     `json:"relay_peers"`
}

// NewApp creates a new App.
func NewApp(cfg *Config) *App {
	ctx, cancel := context.WithCancel(context.Background())
	app := &App{
		Cfg:    cfg,
		Tunnel: New(cfg),
		Ctx:    ctx,
		Cancel: cancel,
	}
	// Set default callback to prevent nil dereference if tray setup is slow
	app.OnConnFailed = func(errMsg string) bool {
		log.Printf("connection failed: %s (retrying)", errMsg)
		return true
	}
	return app
}

// SetTUNFactory sets the platform-specific TUN device factory.
func (a *App) SetTUNFactory(f func(TunConfig) (TunDevice, error)) {
	a.NewTUN = f
}

// GetStatus returns the current status snapshot.
func (a *App) GetStatus() StatusResponse {
	a.Mu.RLock()
	defer a.Mu.RUnlock()

	s := StatusResponse{
		Connected:  a.Connected,
		Connecting: a.Connecting,
		LastError:  a.LastErr,
		PlayerName: a.Cfg.PlayerName,
		RoomID:     a.Cfg.RoomID,
		ServerAddr: a.Cfg.ServerAddr,
		PeerCount:  a.PeerCount,
	}

	if a.Connected {
		s.VirtualIP = a.VirtualIP.String()
		if a.SubnetMask != nil {
			ones, _ := a.SubnetMask.Size()
			s.Subnet = fmt.Sprintf("%s/%d", a.VirtualIP.Mask(a.SubnetMask), ones)
		}
		s.ServerIP = a.ServerIP.String()
		if !a.Uptime.IsZero() {
			s.Uptime = FormatDuration(time.Since(a.Uptime))
		}
		// Connection quality and version from tunnel
		if a.Tunnel != nil {
			ts := a.Tunnel.Status()
			s.AvgRTT = ts.AvgRTT
			s.LossRate = ts.LossRate
			s.P2PPeers = ts.P2PPeers
			s.RelayPeers = ts.RelayPeers

			// Version info
			if ts.ServerVersion > 0 {
				s.ServerVersion = FormatVersion(ts.ServerVersion)
				if ts.ServerVersion > protocol.AppVersion {
					s.UpgradeAvailable = true
					s.UpgradeMessage = fmt.Sprintf(i18n.T().UpgradePrompt,
						FormatVersion(protocol.AppVersion),
						FormatVersion(ts.ServerVersion))
				}
			}
		}
	}

	return s
}

// Connect starts the tunnel connection in a goroutine.
func (a *App) Connect(cfg *Config) {
	a.Mu.Lock()
	if a.Connecting {
		a.Mu.Unlock()
		return
	}
	a.Connecting = true
	a.ConnectGen++
	a.LastErr = ""
	a.Mu.Unlock()

	// Clean up old tunnel before creating new one
	a.Cancel()
	a.Tunnel.Disconnect()
	a.Tunnel.CloseTUN()

	// Update config and create new tunnel (under lock to avoid data race)
	a.Mu.Lock()
	a.Cfg = cfg
	a.Tunnel = New(cfg)
	a.Ctx, a.Cancel = context.WithCancel(context.Background())
	a.Mu.Unlock()

	if err := SaveConfig(cfg); err != nil {
		log.Printf("%s", i18n.Format(i18n.T().AppSaveFail, err))
	}

	go a.connectLoop()
}

// Disconnect stops the tunnel.
func (a *App) Disconnect() {
	a.Mu.Lock()
	a.Cancel()
	oldTun := a.Tunnel
	a.Connected = false
	a.Connecting = false
	a.PeerCount = 0
	a.Ctx, a.Cancel = context.WithCancel(context.Background())
	a.Mu.Unlock()

	oldTun.Disconnect()
	oldTun.CloseTUN()
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
		a.Mu.RLock()
		tun := a.Tunnel
		a.Mu.RUnlock()

		if tun == nil {
			continue
		}

		ts := tun.Status()

		a.Mu.Lock()
		if ts.Connected {
			wasConnected := a.Connected
			a.Connected = true
			a.VirtualIP = ts.VirtualIP
			a.SubnetMask = ts.SubnetMask
			a.ServerIP = ts.ServerIP
			a.PeerCount = ts.PeerCount
			if !wasConnected {
				a.Uptime = time.Now()
				a.LastErr = "" // clear stale error on successful connection
			}
		} else if a.Connected {
			// Was connected, now disconnected
			a.Connected = false
			a.PeerCount = 0
		}
		a.Mu.Unlock()
	}
}

// connectLoop handles connection with auto-reconnect.
// After fastRetries (3) rapid attempts, it pauses and calls OnConnFailed
// to let the user decide: retry, edit settings, or stop.
func (a *App) connectLoop() {
	// Capture config and generation under lock to avoid data race with Connect()
	a.Mu.RLock()
	cfg := a.Cfg
	tun := a.Tunnel
	ctx := a.Ctx
	gen := a.ConnectGen
	a.Mu.RUnlock()

	// Validate server address format before attempting connection
	if err := ValidateServerAddr(cfg.ServerAddr); err != nil {
		log.Printf("Invalid server address: %v", err)
		return
	}

	// Start status polling for this connection session
	pollCtx, pollCancel := context.WithCancel(ctx)
	defer pollCancel()
	go a.statusLoop(pollCtx)

	defer func() {
		a.Mu.Lock()
		// Only reset connecting if this is still the current generation.
		// A newer Connect() call would have incremented gen.
		if a.ConnectGen == gen {
			a.Connecting = false
		}
		a.Mu.Unlock()
	}()

	const (
		baseDelay   = 2 * time.Second
		maxDelay    = 60 * time.Second
		fastRetries = 3 // number of rapid retries before prompting user
	)

	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			// Linear backoff with jitter: 2s, 3s, 4s, 5s... capped at maxDelay.
			// Gentler than exponential for better UX during server restarts.
			delay := baseDelay + time.Duration(attempt)*baseDelay/2
			if delay > maxDelay {
				delay = maxDelay
			}
			// Add ±20% jitter to avoid thundering herd
			jitter := time.Duration(rand.Int63n(int64(delay) / 5))
			delay = delay - delay/10 + jitter
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}

		err := tun.Connect(ctx, cfg.ServerAddr, cfg.MTU, a.NewTUN)
		if ctx.Err() != nil {
			return
		}

		if err != nil {
			errMsg := err.Error()
			a.Mu.Lock()
			a.LastErr = errMsg
			a.Connected = false
			a.Mu.Unlock()
			log.Printf("%s", i18n.Format(i18n.T().AppDisconnectErr, err))

			// After exhausting fast retries, prompt user instead of silent backoff
			if attempt+1 >= fastRetries {
				// Reset attempt counter so next round also gets fast retries
				a.Mu.RLock()
				cb := a.OnConnFailed
				a.Mu.RUnlock()
				if cb != nil {
					shouldRetry := cb(errMsg)
					if !shouldRetry {
						return
					}
					// User chose to retry — reset attempt for another round of fast retries
					attempt = -1
					continue
				}
			}
		} else {
			a.Mu.Lock()
			a.Connected = false
			a.LastErr = ""
			a.Mu.Unlock()
			log.Printf("%s", i18n.T().AppDisconnected)
			// Successful connection that later dropped — reset attempt counter
			attempt = -1
		}
	}
}

// FormatDuration formats a duration as "HH:MM:SS" or "MM:SS".
func FormatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// FormatVersion formats an encoded version number (major<<8|minor) as "vX.Y".
func FormatVersion(v uint16) string {
	return fmt.Sprintf("v%d.%d", protocol.VersionMajor(v), protocol.VersionMinor(v))
}

// JSON serializes the status to JSON bytes.
func (s StatusResponse) JSON() []byte {
	b, err := json.Marshal(s)
	if err != nil {
		log.Printf("status JSON marshal: %v", err)
		return []byte("{}")
	}
	return b
}
