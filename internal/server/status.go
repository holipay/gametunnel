package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/holipay/gametunnel/internal/i18n"
)

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
	mux.HandleFunc("/api/status", s.handleStatusJSON)

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
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
	if t := r.URL.Query().Get("token"); t == s.statusToken {
		return true
	}

	// Check Authorization header (API use)
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		if auth[7:] == s.statusToken {
			return true
		}
	}

	return false
}

func (s *Server) handleStatusJSON(w http.ResponseWriter, r *http.Request) {
	if !s.checkStatusToken(r) {
		http.Error(w, "403 forbidden: invalid or missing token", http.StatusForbidden)
		return
	}
	info := s.buildStatusInfo()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(info)
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

var statusTmplCache struct {
	lang i18n.Lang
	tmpl *template.Template
}

func getStatusTmpl(lang i18n.Lang) *template.Template {
	if statusTmplCache.tmpl != nil && statusTmplCache.lang == lang {
		return statusTmplCache.tmpl
	}
	tmpl := template.Must(template.New("status").Parse(statusHTML))
	statusTmplCache.lang = lang
	statusTmplCache.tmpl = tmpl
	return tmpl
}

const statusHTML = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>{{.T.StatusTitle}}</title>
<meta http-equiv="refresh" content="5">
<style>
  body { font-family: -apple-system, "Segoe UI", sans-serif; max-width: 600px; margin: 40px auto; padding: 0 20px; background: #1a1a2e; color: #e0e0e0; }
  h1 { color: #00d4ff; }
  .stat { display: inline-block; background: #16213e; border-radius: 8px; padding: 12px 20px; margin: 5px; min-width: 120px; text-align: center; }
  .stat .num { font-size: 2em; font-weight: bold; color: #00d4ff; }
  .stat .label { font-size: 0.85em; color: #888; }
  table { width: 100%; border-collapse: collapse; margin-top: 20px; }
  th { text-align: left; padding: 8px; border-bottom: 2px solid #333; color: #00d4ff; }
  td { padding: 8px; border-bottom: 1px solid #2a2a3e; }
  .meta { color: #666; font-size: 0.85em; margin-top: 20px; }
</style>
</head>
<body>
<h1>🎮 {{.T.StatusTitle}}</h1>
<div>
  <div class="stat"><div class="num">{{.Players}}/{{.MaxPlayers}}</div><div class="label">{{.T.StatusPlayers}}</div></div>
  <div class="stat"><div class="num">{{.Uptime}}</div><div class="label">{{.T.StatusUptime}}</div></div>
  <div class="stat"><div class="num">{{.Version}}</div><div class="label">{{.T.StatusVersion}}</div></div>
</div>
{{if .Connections}}
<table><tr><th>{{.T.StatusTablePlayer}}</th><th>{{.T.StatusTableVIP}}</th><th>{{.T.StatusTableAddr}}</th><th>{{.T.StatusTablePing}}</th><th>Loss</th><th>Jitter</th><th>{{.T.StatusTableIdle}}</th></tr>
{{range .Connections}}<tr><td>{{.Username}}</td><td>{{.VirtualIP}}</td><td>{{.PublicAddr}}</td><td>{{.Ping}}</td><td>{{.Loss}}</td><td>{{.Jitter}}</td><td>{{.Idle}}</td></tr>
{{end}}</table>
{{else}}
<p style="color:#666">{{.T.StatusNoPlayers}}</p>
{{end}}
<div class="meta">{{.Subnet}} · {{.ServerIP}} · {{if .HasAuth}}{{.T.StatusAuthHMAC}}{{else}}{{.T.StatusAuthNone}}{{end}}</div>
<h2 style="color:#00d4ff;font-size:1.1em;margin-top:24px">📊 {{.T.StatusMetrics}}</h2>
<div>
  <div class="stat"><div class="num">{{.TotalRegistrations}}</div><div class="label">{{.T.StatusTotalRegs}}</div></div>
  <div class="stat"><div class="num">{{.PeakPlayers}}</div><div class="label">{{.T.StatusPeakPlayers}}</div></div>
  <div class="stat"><div class="num">{{.TotalPacketsRelay}}</div><div class="label">{{.T.StatusRelayPkts}}</div></div>
</div>
<div>
  <div class="stat"><div class="num">{{.AuthFailures}}</div><div class="label">{{.T.StatusAuthFails}}</div></div>
  <div class="stat"><div class="num">{{.TotalKicks}}</div><div class="label">{{.T.StatusTotalKicks}}</div></div>
  <div class="stat"><div class="num">{{.TotalPacketsDropped}}</div><div class="label">{{.T.StatusDroppedPkts}}</div></div>
</div>
<div>
  <div class="stat"><div class="num">{{.SendErrors}}</div><div class="label">{{.T.StatusSendErrors}}</div></div>
</div>
</body></html>`

func (s *Server) buildStatusInfo() StatusInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	t := i18n.T()
	now := time.Now()
	conns := make([]ConnectionInfo, 0, len(s.clients))
	for _, c := range s.clients {
		idle := now.Sub(c.LastSeen)
		idleStr := t.StatusJustNow
		if idle > time.Second {
			idleStr = fmt.Sprintf(t.StatusSecAgo, int(idle.Seconds()))
		}
		pubAddr := ""
		if c.PublicAddr != nil {
			pubAddr = c.PublicAddr.String()
		}
		pingStr := "--"
		if c.RTT > 0 {
			pingStr = fmt.Sprintf("%dms", c.RTT.Milliseconds())
		}
		lossRate, jitter := c.PingStats()
		lossStr := "--"
		if c.pingIdx > 0 {
			lossStr = fmt.Sprintf("%.0f%%", lossRate*100)
		}
		jitterStr := "--"
		if jitter > 0 {
			jitterStr = fmt.Sprintf("%dms", jitter.Milliseconds())
		}
		conns = append(conns, ConnectionInfo{
			Username:   c.Username,
			VirtualIP:  c.VirtualIP.String(),
			PublicAddr: pubAddr,
			Idle:       idleStr,
			Ping:       pingStr,
			Loss:       lossStr,
			Jitter:     jitterStr,
		})
	}

	uptime := now.Sub(s.startTime)

	return StatusInfo{
		Version:     s.version,
		Uptime:      formatDuration(uptime),
		Players:     len(s.clients),
		MaxPlayers:  s.maxPlayers,
		Subnet:      s.subnet.String(),
		ServerIP:    s.serverIP.String(),
		HasAuth:     s.roomPass != "",
		SendErrors:  s.sendErrors.Load(),
		Connections: conns,

		TotalRegistrations:  s.totalRegistrations.Load(),
		AuthFailures:        s.authFailures.Load(),
		PeakPlayers:         s.peakPlayers.Load(),
		TotalPacketsRelay:   s.totalPacketsRelay.Load(),
		TotalPacketsDropped: s.totalPacketsDropped.Load(),
		TotalKicks:          s.totalKicks.Load(),
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
