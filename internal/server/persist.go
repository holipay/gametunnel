package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/holipay/gametunnel/internal/i18n"
)

const (
	// stateFileName is the name of the state file on disk.
	stateFileName = "room-state.json"

	// stateVersion tracks the on-disk format. Bump on breaking changes.
	stateVersion = 1

	// reconnectGracePeriod is how long after a restart we keep IP reservations
	// for clients that haven't reconnected yet.
	reconnectGracePeriod = 60 * time.Second

	// persistDebounceInterval is how often we write state to disk at most.
	// Changes within this window are coalesced into a single write.
	persistDebounceInterval = 30 * time.Second
)

// ── On-disk format ──────────────────────────────────────────────

// RoomState is the JSON-serializable room state saved to disk.
type RoomState struct {
	Version   int                    `json:"version"`
	Subnet    string                 `json:"subnet"`
	UpdatedAt time.Time              `json:"updated_at"`
	IPBitmap  []uint64               `json:"ip_bitmap"`
	Clients   map[string]ClientEntry `json:"clients"` // key = virtualIP string
}

// ClientEntry is the persisted subset of a Client.
// PublicAddr is NOT persisted — clients re-register after restart.
type ClientEntry struct {
	Username  string    `json:"username"`
	VirtualIP string    `json:"virtual_ip"`
	LastSeen  time.Time `json:"last_seen"`
}

// ── Persist helper (attached to Server) ─────────────────────────

// loadState loads room state from disk and restores IP reservations.
// Called once during New(). Returns nil (no-op) if no state file exists.
func (s *Server) loadState() error {
	if s.stateDir == "" {
		return nil
	}

	// Only load state for single-room mode
	if s.defaultRoom == nil {
		return nil
	}

	path := filepath.Join(s.stateDir, stateFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // first run, nothing to load
		}
		return fmt.Errorf("read state file: %w", err)
	}

	var state RoomState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("parse state file: %w", err)
	}

	// Validate
	if state.Version != stateVersion {
		log.Printf("ignoring state file: version %d != %d", state.Version, stateVersion)
		return nil
	}

	room := s.defaultRoom
	if state.Subnet != room.subnet.String() {
		log.Printf("ignoring state file: subnet %s != %s", state.Subnet, room.subnet.String())
		return nil
	}

	now := time.Now()
	restored := 0

	room.mu.Lock()
	for _, entry := range state.Clients {
		ip := net.ParseIP(entry.VirtualIP)
		if ip == nil || ip.To4() == nil {
			continue
		}

		// Skip clients that were already stale before the restart
		if now.Sub(entry.LastSeen) > 30*time.Second {
			// But if within grace period, still reserve the IP
			if now.Sub(state.UpdatedAt) > reconnectGracePeriod {
				continue
			}
		}

		// Check IP is within subnet
		if !room.subnet.Contains(ip) {
			continue
		}

		octet := ip.To4()[3]
		if octet < 2 || octet >= 255 {
			continue
		}

		// Reserve IP in bitmap
		if room.ipBitmap[octet/64]&(1<<(octet%64)) != 0 {
			continue // already taken (e.g. server IP)
		}

		room.markIPUsed(ip)

		// Create a placeholder client entry. The client will get a fresh
		// PublicAddr when it reconnects; until then it shows as "offline".
		c := &Client{
			Username:   entry.Username,
			VirtualIP:  ip,
			auth:       authNone,
			authRoomID: "default", // single-room mode uses "default" roomID
		}
		c.SetLastSeen(entry.LastSeen)
		room.clients[ipKey(ip)] = c
		// NOTE: do NOT add to addrMap yet — no PublicAddr until reconnect
		restored++
	}
	room.mu.Unlock()

	if restored > 0 {
		log.Printf(i18n.T().LogStateRestored, restored)
	}

	// Update the in-memory updatedAt so we know when the state was loaded
	s.stateLoadedAt = now
	return nil
}

// persistLoop runs in a background goroutine and writes state to disk
// when the dirty flag is set, debounced to persistDebounceInterval.
func (s *Server) persistLoop(ctx context.Context) {
	if s.stateDir == "" {
		return
	}

	ticker := time.NewTicker(persistDebounceInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Final save on shutdown
			s.saveState()
			return
		case <-ticker.C:
			if s.persistDirty.CompareAndSwap(true, false) {
				s.saveState()
			}
		}
	}
}

// saveState writes the current room state to disk atomically.
func (s *Server) saveState() {
	if s.stateDir == "" {
		return
	}

	// Only save state for single-room mode
	if s.defaultRoom == nil {
		return
	}

	state := s.defaultRoom.SnapshotState()

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		log.Printf("persist: marshal state: %v", err)
		return
	}

	path := filepath.Join(s.stateDir, stateFileName)
	tmpPath := path + ".tmp"

	// Atomic write: write to tmp, then rename
	if err := os.MkdirAll(s.stateDir, 0755); err != nil {
		log.Printf("persist: mkdir: %v", err)
		return
	}
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		log.Printf("persist: write tmp: %v", err)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		log.Printf("persist: rename: %v", err)
		os.Remove(tmpPath)
		return
	}
}
