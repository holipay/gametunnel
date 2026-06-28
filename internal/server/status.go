package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"crypto/subtle"

	"github.com/holipay/gametunnel/internal/i18n"
)

//go:embed embed/status.html
var statusHTML string

// StatusInfo is the JSON response from the status API.
type StatusInfo struct {
	Version     string           `json:"version"`
	Uptime      string           `json:"uptime"`
	Players     int              `json:"players"`
	MaxPlayers  int              `json:"max_players"`
	Subnet      string           `json:"subnet"`
	ServerIP    string           `json:"server_ip"`
	HasAuth     bool             `json:"has_auth"`
	SendErrors  int64            `json:"send_errors"`
	Connections []ConnectionInfo `json:"connections,omitempty"`
	MultiRoom   bool             `json:"multi_room"`
	Rooms       []RoomStatusInfo `json:"rooms,omitempty"`

	// Operational metrics (lifetime counters)
	TotalRegistrations uint64 `json:"total_registrations"`
	AuthFailures       uint64 `json:"auth_failures"`
	PeakPlayers        uint32 `json:"peak_players"`
	TotalPacketsRelay  uint64 `json:"total_packets_relay"`
	TotalPacketsDropped uint64 `json:"total_packets_dropped"`
	TotalKicks         uint64 `json:"total_kicks"`
}

// ConnectionInfo describes a single connected player.
type ConnectionInfo struct {
	Username   string `json:"username"`
	VirtualIP  string `json:"virtual_ip"`
	PublicAddr string `json:"public_addr"`
	Idle       string `json:"idle"`
	Ping       string `json:"ping"`
	Loss       string `json:"loss"`
	Jitter     string `json:"jitter"`
}

func (s *Server) startStatusServer(ctx context.Context, addr string) {
	if addr == "" {
		return
	}

	// 自动补全地址格式: "4701" → ":4701"
	if !strings.Contains(addr, ":") {
		addr = ":" + addr
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleStatusHTML)
	mux.HandleFunc("/api/status", s.requireToken(s.handleStatusJSON))
	mux.HandleFunc("/api/metrics", s.requireToken(s.handleMetricsJSON))
	mux.HandleFunc("/api/rooms", s.requireToken(s.handleRoomsJSON))

	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("%s", i18n.Format(i18n.T().ServerStatusLog, addr))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("%s", i18n.Format(i18n.T().ServerStatusFail, err))
		}
	}()

	go func() {
		<-ctx.Done()
		srv.Close()
	}()
}

// checkStatusToken validates the request token against the configured
// statusToken. Returns true if no token is configured (open access) or
// if the token matches. Supports query param ?token=xxx and
// Authorization: Bearer xxx header.
func (s *Server) checkStatusToken(r *http.Request) bool {
	if s.statusToken == "" {
		return true // no token configured, open access
	}

	// Check query parameter (works for both HTML and API)
	if t := r.URL.Query().Get("token"); subtle.ConstantTimeCompare([]byte(t), []byte(s.statusToken)) == 1 {
		return true
	}

	// Check Authorization header (API use)
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		if subtle.ConstantTimeCompare([]byte(auth[7:]), []byte(s.statusToken)) == 1 {
			return true
		}
	}

	return false
}

// requireToken wraps an HTTP handler with token authentication.
func (s *Server) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.checkStatusToken(r) {
			http.Error(w, "403 forbidden: invalid or missing token", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// writeJSON encodes v as JSON and writes it to w.
func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("JSON encode: %v", err)
	}
}

func (s *Server) handleStatusJSON(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.buildStatusInfo())
}

// MetricsAPIResponse is the JSON response from /api/metrics.
type MetricsAPIResponse struct {
	Interval string          `json:"interval"` // e.g. "1m"
	Window   string          `json:"window"`   // e.g. "1h"
	Samples  []MetricsSample `json:"samples"`
}

func (s *Server) handleMetricsJSON(w http.ResponseWriter, r *http.Request) {
	resp := MetricsAPIResponse{
		Interval: "1m",
		Window:   "1h",
		Samples:  s.metricsTS.Snapshot(),
	}
	writeJSON(w, resp)
}

func (s *Server) handleRoomsJSON(w http.ResponseWriter, r *http.Request) {
	if !s.multiRoom {
		http.Error(w, "multi-room not enabled", http.StatusBadRequest)
		return
	}
	s.roomMu.RLock()
	rooms := make([]RoomStatusInfo, 0, len(s.rooms))
	for _, room := range s.rooms {
		rooms = append(rooms, room.BuildRoomStatus())
	}
	s.roomMu.RUnlock()
	writeJSON(w, rooms)
}

func (s *Server) handleStatusHTML(w http.ResponseWriter, r *http.Request) {
	if !s.checkStatusToken(r) {
		http.Error(w, "403 forbidden: invalid or missing token", http.StatusForbidden)
		return
	}
	info := s.buildStatusInfo()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	t := i18n.T()
	tmplData := struct {
		*StatusInfo
		T *i18n.Strings
	}{StatusInfo: &info, T: t}

	if err := getStatusTmpl(s.lang).Execute(w, tmplData); err != nil {
		log.Printf("%s", i18n.Format(i18n.T().ServerTmplFail, err))
	}
}

var (
	statusTmplOnce sync.Once
	statusTmpl     *template.Template
)

func getStatusTmpl(lang i18n.Lang) *template.Template {
	statusTmplOnce.Do(func() {
		statusTmpl = template.Must(template.New("status").Parse(statusHTML))
	})
	return statusTmpl
}

func (s *Server) buildStatusInfo() StatusInfo {
	now := time.Now()

	// Collect connections from default room (single-room) or all rooms (multi-room)
	var conns []ConnectionInfo
	var totalPlayers int
	var maxPlayers int
	var subnet string
	var serverIP string
	var hasAuth bool
	var totalRegistrations uint64
	var authFailures uint64
	var peakPlayers uint32
	var totalPacketsRelay uint64
	var totalPacketsDropped uint64
	var totalKicks uint64

	// Multi-room: collect all rooms in a single lock acquisition
	var roomInfos []RoomStatusInfo
	if s.multiRoom {
		s.roomMu.RLock()
		for _, room := range s.rooms {
			status := room.BuildRoomStatus()
			roomInfos = append(roomInfos, status)
			conns = append(conns, status.Connections...)
			totalPlayers += status.Players
			maxPlayers += status.MaxPlayers
			totalRegistrations += status.TotalRegistrations
			authFailures += status.AuthFailures
			if status.PeakPlayers > peakPlayers {
				peakPlayers = status.PeakPlayers
			}
			totalPacketsRelay += status.TotalPacketsRelay
			totalPacketsDropped += status.TotalPacketsDropped
			totalKicks += status.TotalKicks
		}
		s.roomMu.RUnlock()
		if s.defaultRoom != nil {
			subnet = s.defaultRoom.subnet.String()
			serverIP = s.defaultRoom.serverIP.String()
		}
	} else if s.defaultRoom != nil {
		// Single-room: get from default room
		status := s.defaultRoom.BuildRoomStatus()
		conns = status.Connections
		totalPlayers = status.Players
		maxPlayers = status.MaxPlayers
		subnet = s.defaultRoom.subnet.String()
		serverIP = s.defaultRoom.serverIP.String()
		hasAuth = s.defaultRoom.roomPass != ""
		totalRegistrations = status.TotalRegistrations
		authFailures = status.AuthFailures
		peakPlayers = status.PeakPlayers
		totalPacketsRelay = status.TotalPacketsRelay
		totalPacketsDropped = status.TotalPacketsDropped
		totalKicks = status.TotalKicks
	}

	uptime := now.Sub(s.startTime)

	return StatusInfo{
		Version:     s.version,
		Uptime:      formatDuration(uptime),
		Players:     totalPlayers,
		MaxPlayers:  maxPlayers,
		Subnet:      subnet,
		ServerIP:    serverIP,
		HasAuth:     hasAuth,
		SendErrors:  s.sendErrors.Load(),
		Connections: conns,
		MultiRoom:   s.multiRoom,
		Rooms:       roomInfos,

		TotalRegistrations:  totalRegistrations,
		AuthFailures:        authFailures,
		PeakPlayers:         peakPlayers,
		TotalPacketsRelay:   totalPacketsRelay,
		TotalPacketsDropped: totalPacketsDropped,
		TotalKicks:          totalKicks,
	}
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h < 24 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	days := h / 24
	h = h % 24
	return fmt.Sprintf("%dd%dh", days, h)
}
