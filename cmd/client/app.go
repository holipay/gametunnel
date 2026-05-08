package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/holipay/gametunnel/internal/client"
)

// App wraps the tunnel with HTTP API and status tracking.
type App struct {
	cfg       *client.Config
	cfgPath   string
	tunnel    *client.Tunnel
	mu        sync.RWMutex
	connecting bool
	lastErr   string

	// Status
	connected   bool
	virtualIP   net.IP
	subnetMask  net.IPMask
	serverIP    net.IP
	peers       []PeerStatus
	rtt         time.Duration
	uptime      time.Time
	connCount   int

	// TUN factory (set by platform-specific main)
	newTUN func(client.TunConfig) (client.TunDevice, error)

	// Context for connect loop
	ctx    context.Context
	cancel context.CancelFunc
}

// PeerStatus is the JSON-serializable peer info.
type PeerStatus struct {
	Username  string `json:"username"`
	VirtualIP string `json:"virtual_ip"`
	RTT       string `json:"rtt"`
}

// StatusResponse is the SSE status payload.
type StatusResponse struct {
	Connected  bool         `json:"connected"`
	Connecting bool         `json:"connecting"`
	LastError  string       `json:"last_error,omitempty"`
	VirtualIP  string       `json:"virtual_ip,omitempty"`
	Subnet     string       `json:"subnet,omitempty"`
	ServerIP   string       `json:"server_ip,omitempty"`
	RTT        int64        `json:"rtt,omitempty"`
	Uptime     string       `json:"uptime,omitempty"`
	Peers      []PeerStatus `json:"peers"`
	PlayerName string       `json:"player_name"`
	RoomID     string       `json:"room_id"`
	ServerAddr string       `json:"server_addr"`
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
		Peers:      a.peers,
	}

	if a.connected {
		s.VirtualIP = a.virtualIP.String()
		if a.subnetMask != nil {
			ones, _ := a.subnetMask.Size()
			s.Subnet = fmt.Sprintf("%s/%d", a.virtualIP.Mask(a.subnetMask), ones)
		}
		s.ServerIP = a.serverIP.String()
		if a.rtt > 0 {
			s.RTT = a.rtt.Milliseconds()
		}
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

	// Update config
	a.cfg = cfg
	a.tunnel = client.New(cfg)
	client.SaveConfig(cfg)

	go a.connectLoop()
}

// Disconnect stops the tunnel.
func (a *App) Disconnect() {
	a.cancel()
	a.tunnel.Disconnect()

	a.mu.Lock()
	a.connected = false
	a.connecting = false
	a.peers = nil
	a.mu.Unlock()

	// Reset context for next connection
	a.ctx, a.cancel = context.WithCancel(context.Background())
}

// connectLoop handles connection with auto-reconnect.
func (a *App) connectLoop() {
	defer func() {
		a.mu.Lock()
		a.connecting = false
		a.mu.Unlock()
	}()

	const (
		baseDelay = 2 * time.Second
		maxDelay  = 60 * time.Second
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
			a.mu.Lock()
			a.lastErr = err.Error()
			a.connected = false
			a.mu.Unlock()
			log.Printf("[app] 连接断开: %v", err)
		} else {
			a.mu.Lock()
			a.connected = false
			a.lastErr = ""
			a.mu.Unlock()
			log.Printf("[app] 连接断开")
		}
	}
}

// updateStatus is called by the tunnel callbacks to refresh status.
func (a *App) updateStatus(connected bool, vip net.IP, mask net.IPMask, serverIP net.IP) {
	a.mu.Lock()
	defer a.mu.Unlock()

	wasConnected := a.connected
	a.connected = connected

	if connected {
		a.virtualIP = vip
		a.subnetMask = mask
		a.serverIP = serverIP
		if !wasConnected {
			a.uptime = time.Now()
			a.connCount++
		}
	} else {
		a.peers = nil
		a.rtt = 0
	}
}

// updatePeers refreshes the peer list.
func (a *App) updatePeers(peers []PeerStatus) {
	a.mu.Lock()
	a.peers = peers
	a.mu.Unlock()
}

// updateRTT updates the latency measurement.
func (a *App) updateRTT(d time.Duration) {
	a.mu.Lock()
	a.rtt = d
	a.mu.Unlock()
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
