package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// StatusInfo is the JSON response from the status API.
type StatusInfo struct {
	Version    string           `json:"version"`
	Uptime     string           `json:"uptime"`
	Players    int              `json:"players"`
	MaxPlayers int              `json:"max_players"`
	Subnet     string           `json:"subnet"`
	ServerIP   string           `json:"server_ip"`
	HasAuth    bool             `json:"has_auth"`
	Connections []ConnectionInfo `json:"connections,omitempty"`
}

// ConnectionInfo describes a single connected player.
type ConnectionInfo struct {
	Username   string `json:"username"`
	VirtualIP  string `json:"virtual_ip"`
	PublicAddr string `json:"public_addr"`
	Idle       string `json:"idle"`
}

func (s *Server) startStatusServer(addr string) {
	if addr == "" {
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleStatusHTML)
	mux.HandleFunc("/api/status", s.handleStatusJSON)

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	go func() {
		log.Printf("[status] 状态页面: http://%s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[status] HTTP 服务启动失败: %v", err)
		}
	}()
}

func (s *Server) handleStatusJSON(w http.ResponseWriter, r *http.Request) {
	info := s.buildStatusInfo()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(info)
}

func (s *Server) handleStatusHTML(w http.ResponseWriter, r *http.Request) {
	info := s.buildStatusInfo()

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>GameTunnel Server</title>
<meta http-equiv="refresh" content="5">
<style>
  body { font-family: -apple-system, "Segoe UI", sans-serif; max-width: 600px; margin: 40px auto; padding: 0 20px; background: #1a1a2e; color: #e0e0e0; }
  h1 { color: #00d4ff; }
  .stat { display: inline-block; background: #16213e; border-radius: 8px; padding: 12px 20px; margin: 5px; min-width: 120px; text-align: center; }
  .stat .num { font-size: 2em; font-weight: bold; color: #00d4ff; }
  .stat .label { font-size: 0.85em; color: #888; }
  table { width: 100%%; border-collapse: collapse; margin-top: 20px; }
  th { text-align: left; padding: 8px; border-bottom: 2px solid #333; color: #00d4ff; }
  td { padding: 8px; border-bottom: 1px solid #2a2a3e; }
  .meta { color: #666; font-size: 0.85em; margin-top: 20px; }
</style>
</head>
<body>
<h1>🎮 GameTunnel Server</h1>
<div>
  <div class="stat"><div class="num">%d/%d</div><div class="label">玩家</div></div>
  <div class="stat"><div class="num">%s</div><div class="label">运行时间</div></div>
  <div class="stat"><div class="num">%s</div><div class="label">版本</div></div>
</div>
`, info.Players, info.MaxPlayers, info.Uptime, info.Version)

	if len(info.Connections) > 0 {
		fmt.Fprint(w, `<table><tr><th>玩家</th><th>虚拟 IP</th><th>地址</th><th>空闲</th></tr>`)
		for _, c := range info.Connections {
			fmt.Fprintf(w, `<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`,
				c.Username, c.VirtualIP, c.PublicAddr, c.Idle)
		}
		fmt.Fprint(w, `</table>`)
	} else {
		fmt.Fprint(w, `<p style="color:#666">暂无玩家连接</p>`)
	}

	fmt.Fprintf(w, `<div class="meta">%s · %s · %s</div>`,
		info.Subnet, info.ServerIP, map[bool]string{true: "HMAC 认证", false: "无认证"}[info.HasAuth])
	fmt.Fprint(w, `</body></html>`)
}

func (s *Server) buildStatusInfo() StatusInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := time.Now()
	conns := make([]ConnectionInfo, 0, len(s.clients))
	for _, c := range s.clients {
		idle := now.Sub(c.LastSeen)
		idleStr := "刚刚"
		if idle > time.Second {
			idleStr = fmt.Sprintf("%ds前", int(idle.Seconds()))
		}
		pubAddr := ""
		if c.PublicAddr != nil {
			pubAddr = c.PublicAddr.String()
		}
		conns = append(conns, ConnectionInfo{
			Username:   c.Username,
			VirtualIP:  c.VirtualIP.String(),
			PublicAddr: pubAddr,
			Idle:       idleStr,
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
		Connections: conns,
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
