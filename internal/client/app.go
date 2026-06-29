// Package client provides the GameTunnel client tunnel and application logic.
// This file contains the App struct which wraps Tunnel with connection management,
// status tracking, and auto-reconnect. It is platform-independent.
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/protocol"
)

// App wraps the tunnel with connection management, status tracking, and auto-reconnect.
// Platform-specific code (tray, web UI) accesses App through exported fields/methods.
type App struct {
	Cfg        *Config
	Tunnel     *Tunnel
	Mu         sync.RWMutex
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
				s.ServerVersion = protocol.FormatVersion(ts.ServerVersion)
				if ts.ServerVersion > protocol.AppVersion {
					s.UpgradeAvailable = true
					s.UpgradeMessage = fmt.Sprintf(i18n.T().UpgradePrompt,
						protocol.FormatVersion(protocol.AppVersion),
						protocol.FormatVersion(ts.ServerVersion))
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
	// Capture old tunnel under lock before creating new one, so that
	// a concurrent Disconnect() call sees a consistent snapshot.
	oldTun := a.Tunnel
	a.Cancel()

	// Update config and create new tunnel
	a.Cfg = cfg
	a.Tunnel = New(cfg)
	a.Ctx, a.Cancel = context.WithCancel(context.Background())
	a.Mu.Unlock()

	// Clean up old tunnel after releasing lock (may block on I/O)
	oldTun.Disconnect()
	oldTun.CloseTUN()

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
//
// Uses SmartBackoff to choose reconnect delay based on disconnect reason:
//   - Network glitch (was connected, lost): 500ms fast retry → exponential
//   - Server unreachable: 2s → 4s → 8s → ... → 60s exponential
//   - DNS failure: 5s → 15s → 45s → 60s
//   - Server full: 10s → 20s → 40s → 60s
//   - Fatal (wrong password, version mismatch): stop immediately
func (a *App) connectLoop() {
	a.Mu.RLock()
	cfg := a.Cfg
	tun := a.Tunnel
	ctx := a.Ctx
	gen := a.ConnectGen
	a.Mu.RUnlock()

	if err := ValidateServerAddr(cfg.ServerAddr); err != nil {
		log.Printf("Invalid server address: %v", err)
		return
	}

	pollCtx, pollCancel := context.WithCancel(ctx)
	defer pollCancel()
	go a.statusLoop(pollCtx)

	defer func() {
		a.Mu.Lock()
		if a.ConnectGen == gen {
			a.Connecting = false
		}
		a.Mu.Unlock()
	}()

	const fastRetries = 3
	backoff := NewSmartBackoff(DisconnectReasonUnknown, false)

	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			delay := backoff.Next()
			if delay < 0 {
				log.Printf("reconnect: giving up after %d attempts", attempt)
				return
			}

			// Network availability check (skip for fast retries)
			if attempt > fastRetries && !IsNetworkAvailable(cfg.ServerAddr) {
				log.Printf("reconnect: network unavailable, waiting...")
				// Wait a bit and re-check
				netTimer := time.NewTimer(5 * time.Second)
				select {
				case <-ctx.Done():
					netTimer.Stop()
					return
				case <-netTimer.C:
				}
				if !IsNetworkAvailable(cfg.ServerAddr) {
					log.Printf("reconnect: network still unavailable, retrying anyway")
				}
			}

			delayTimer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				delayTimer.Stop()
				return
			case <-delayTimer.C:
			}
		}

		err := tun.Connect(ctx, cfg.ServerAddr, cfg.MTU, a.NewTUN)
		if ctx.Err() != nil {
			return
		}

		// Fatal kick — stop immediately
		if tun.cancelKicks.Load() {
			a.Mu.Lock()
			a.LastErr = "kicked by server (non-recoverable)"
			a.Connected = false
			a.Mu.Unlock()
			log.Printf("server kick: stopping reconnect")
			return
		}

		if err != nil {
			a.Mu.Lock()
			a.LastErr = err.Error()
			a.Connected = false
			a.Mu.Unlock()
			log.Printf("%s", i18n.Format(i18n.T().AppDisconnectErr, err))

			// Classify error and set appropriate backoff strategy
			reason := ClassifyError(err)
			if reason == DisconnectReasonFatal {
				log.Printf("reconnect: fatal error, stopping")
				return
			}
			if reason == DisconnectReasonServerFull {
				log.Printf("reconnect: server full, backing off")
			}
			backoff = NewSmartBackoff(reason, false)

			if attempt+1 >= fastRetries {
				a.Mu.RLock()
				cb := a.OnConnFailed
				a.Mu.RUnlock()
				if cb != nil && !cb(err.Error()) {
					return
				}
			}
		} else {
			// Was connected, now disconnected — fast reconnect
			a.Mu.Lock()
			a.Connected = false
			a.LastErr = ""
			a.Mu.Unlock()
			log.Printf("%s", i18n.T().AppDisconnected)

			// Use network glitch strategy for fast reconnect
			backoff = NewSmartBackoff(DisconnectReasonNetworkGlitch, true)
			attempt = -1 // reset attempt counter for fast reconnect phase
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

// JSON serializes the status to JSON bytes.
func (s StatusResponse) JSON() []byte {
	b, err := json.Marshal(s)
	if err != nil {
		log.Printf("status JSON marshal: %v", err)
		return []byte("{}")
	}
	return b
}
