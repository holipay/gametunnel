package client

import "fmt"

// ── Tray State ─────────────────────────────────────────────────

// TrayState represents the connection state for tray display.
type TrayState int

const (
	TrayDisconnected TrayState = iota
	TrayConnecting
	TrayConnected
	TrayConnectedP2P
	TrayConnectedRelay
	TrayError
)

// TrayDisplay holds the computed display information for the system tray.
// Platform-specific code (systray, etc.) reads this to update the UI.
type TrayDisplay struct {
	State      TrayState
	IP         string
	Peers      int
	P2PPeers   int
	RelayPeers int
	AvgRTT     float64
	LossRate   float64
	ErrorMsg   string
}

// StatusText returns the human-readable status line for the tray menu.
func (td *TrayDisplay) StatusText() string {
	switch td.State {
	case TrayConnected, TrayConnectedP2P, TrayConnectedRelay:
		s := fmt.Sprintf("%s (%d)", td.IP, td.Peers)
		if td.P2PPeers > 0 || td.RelayPeers > 0 {
			s += fmt.Sprintf("  [P2P:%d Relay:%d]", td.P2PPeers, td.RelayPeers)
		}
		return s
	case TrayConnecting:
		return "..."
	case TrayError:
		return td.ErrorMsg
	default:
		return ""
	}
}

// Tooltip returns the full tooltip text for the tray icon.
func (td *TrayDisplay) Tooltip(title string) string {
	switch td.State {
	case TrayConnected, TrayConnectedP2P, TrayConnectedRelay:
		tip := fmt.Sprintf("%s (%d)", td.IP, td.Peers)
		if td.P2PPeers > 0 || td.RelayPeers > 0 {
			tip += fmt.Sprintf("\nP2P: %d  Relay: %d", td.P2PPeers, td.RelayPeers)
		}
		if td.AvgRTT > 0 {
			tip += fmt.Sprintf("\nRTT: %.0fms", td.AvgRTT)
		}
		if td.LossRate > 0 {
			tip += fmt.Sprintf("\nLoss: %.0f%%", td.LossRate*100)
		}
		return tip
	case TrayError:
		return td.ErrorMsg
	default:
		return title
	}
}

// ── Tray State Manager ─────────────────────────────────────────

// TrayStateManager computes tray display state from App state.
// Platform-specific tray code calls GetDisplay() to determine what to render.
type TrayStateManager struct {
	app *App
}

// NewTrayStateManager creates a manager for the given App.
func NewTrayStateManager(app *App) *TrayStateManager {
	return &TrayStateManager{app: app}
}

// GetDisplay computes the current tray display state.
func (tsm *TrayStateManager) GetDisplay() TrayDisplay {
	tsm.app.Mu.RLock()
	defer tsm.app.Mu.RUnlock()

	// Check for error first
	if tsm.app.LastErr != "" {
		return TrayDisplay{
			State:    TrayError,
			ErrorMsg: tsm.app.LastErr,
		}
	}

	// Check if connecting
	if tsm.app.Connecting {
		return TrayDisplay{State: TrayConnecting}
	}

	// Check if connected
	if !tsm.app.Connected {
		return TrayDisplay{State: TrayDisconnected}
	}

	// Connected — determine connection type
	ts := tsm.app.Tunnel.Status()
	display := TrayDisplay{
		State:      TrayConnected,
		IP:         tsm.app.VirtualIP.String(),
		Peers:      tsm.app.PeerCount,
		P2PPeers:   ts.P2PPeers,
		RelayPeers: ts.RelayPeers,
		AvgRTT:     ts.AvgRTT,
		LossRate:   ts.LossRate,
	}

	if ts.P2PPeers > 0 && ts.RelayPeers == 0 {
		display.State = TrayConnectedP2P
	} else if ts.RelayPeers > 0 {
		display.State = TrayConnectedRelay
	}

	return display
}

// IsConnected returns true if the tray should show connected state.
func (td *TrayDisplay) IsConnected() bool {
	return td.State == TrayConnected || td.State == TrayConnectedP2P || td.State == TrayConnectedRelay
}

// IconHint returns a string identifying the icon to use.
func (td *TrayDisplay) IconHint() string {
	switch td.State {
	case TrayConnectedP2P:
		return "connected_p2p"
	case TrayConnectedRelay:
		return "connected_relay"
	case TrayConnected:
		return "connected"
	case TrayConnecting:
		return "connecting"
	case TrayError:
		return "disconnected"
	default:
		return "disconnected"
	}
}
