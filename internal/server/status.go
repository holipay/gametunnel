package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"sync"
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
	mux.HandleFunc("/api/status", s.handleStatusJSON)
	mux.HandleFunc("/api/metrics", s.handleMetricsJSON)
	mux.HandleFunc("/api/rooms", s.handleRoomsJSON)

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

// MetricsAPIResponse is the JSON response from /api/metrics.
type MetricsAPIResponse struct {
	Interval string          `json:"interval"` // e.g. "1m"
	Window   string          `json:"window"`   // e.g. "1h"
	Samples  []MetricsSample `json:"samples"`
}

func (s *Server) handleMetricsJSON(w http.ResponseWriter, r *http.Request) {
	if !s.checkStatusToken(r) {
		http.Error(w, "403 forbidden: invalid or missing token", http.StatusForbidden)
		return
	}
	resp := MetricsAPIResponse{
		Interval: "1m",
		Window:   "1h",
		Samples:  s.metricsTS.Snapshot(),
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleRoomsJSON(w http.ResponseWriter, r *http.Request) {
	if !s.checkStatusToken(r) {
		http.Error(w, "403 forbidden: invalid or missing token", http.StatusForbidden)
		return
	}
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
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(rooms)
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

<h2 style="color:#00d4ff;font-size:1.1em;margin-top:24px">📈 <span id="charts-title">Charts</span></h2>
<div id="charts-container" style="margin-top:12px">
  <div style="display:flex;gap:12px;flex-wrap:wrap">
    <div style="flex:1;min-width:260px;background:#16213e;border-radius:8px;padding:12px">
      <div style="color:#888;font-size:0.85em;margin-bottom:4px">Players Online</div>
      <canvas id="chart-players" height="80"></canvas>
    </div>
    <div style="flex:1;min-width:260px;background:#16213e;border-radius:8px;padding:12px">
      <div style="color:#888;font-size:0.85em;margin-bottom:4px">Avg RTT (ms)</div>
      <canvas id="chart-rtt" height="80"></canvas>
    </div>
  </div>
  <div style="display:flex;gap:12px;flex-wrap:wrap;margin-top:12px">
    <div style="flex:1;min-width:260px;background:#16213e;border-radius:8px;padding:12px">
      <div style="color:#888;font-size:0.85em;margin-bottom:4px">Avg Loss Rate</div>
      <canvas id="chart-loss" height="80"></canvas>
    </div>
    <div style="flex:1;min-width:260px;background:#16213e;border-radius:8px;padding:12px">
      <div style="color:#888;font-size:0.85em;margin-bottom:4px">Relay Packets/min</div>
      <canvas id="chart-relay" height="80"></canvas>
    </div>
  </div>
</div>
<script>
(function(){
  var ids={p:'chart-players',r:'chart-rtt',l:'chart-loss',rp:'chart-relay'};
  var colors={p:'#00d4ff',r:'#ff6b6b',l:'#ffd93d',rp:'#6bcb77'};
  function draw(id,data,color,pct){
    var c=document.getElementById(id);if(!c||!data.length)return;
    var ctx=c.getContext('2d'),D=window.devicePixelRatio||1;
    var W=c.width=c.offsetWidth*D,H=c.height=c.offsetHeight*D;
    ctx.clearRect(0,0,W,H);
    var mx=0;for(var i=0;i<data.length;i++)if(data[i]>mx)mx=data[i];
    if(mx===0)mx=1;var p=4,gw=W-p*2,gh=H-p*2;
    ctx.strokeStyle='rgba(255,255,255,0.06)';ctx.lineWidth=1;
    for(var g=0;g<4;g++){var gy=p+gh*g/3;ctx.beginPath();ctx.moveTo(p,gy);ctx.lineTo(W-p,gy);ctx.stroke();}
    ctx.strokeStyle=color;ctx.lineWidth=2*D;ctx.lineJoin='round';ctx.beginPath();
    for(var i=0;i<data.length;i++){var x=p+(i/(data.length-1||1))*gw,y=p+gh-(data[i]/mx)*gh;i===0?ctx.moveTo(x,y):ctx.lineTo(x,y);}
    ctx.stroke();ctx.lineTo(p+gw,p+gh);ctx.lineTo(p,p+gh);ctx.closePath();
    ctx.fillStyle=color.replace(')',',0.1)').replace('rgb','rgba');ctx.fill();
    ctx.fillStyle='#666';ctx.font=(10*D)+'px sans-serif';ctx.textAlign='right';
    ctx.fillText(pct?(mx*100).toFixed(0)+'%':mx.toFixed(mx<10?1:0),W-p,p+12*D);
  }
  function go(){
    var x=new XMLHttpRequest();x.open('GET','/api/metrics'+(window.location.search||''),true);
    x.onload=function(){if(x.status!==200)return;var r=JSON.parse(x.responseText),s=r.samples||[];
    if(!s.length)return;var f={p:[],r:[],l:[],rp:[]};
    for(var i=0;i<s.length;i++){f.p.push(s[i].p);f.r.push(s[i].r);f.l.push(s[i].l);f.rp.push(s[i].rp);}
    for(var k in ids)draw(ids[k],f[k],colors[k],k==='l');};x.send();}
  go();setInterval(go,60000);window.addEventListener('resize',go);
})();
</script>
</body></html>`

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

	if s.multiRoom {
		// Multi-room: aggregate from all rooms
		s.roomMu.RLock()
		for _, room := range s.rooms {
			status := room.BuildRoomStatus()
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

	// Multi-room: collect all rooms
	var roomInfos []RoomStatusInfo
	if s.multiRoom {
		s.roomMu.RLock()
		for _, room := range s.rooms {
			roomInfos = append(roomInfos, room.BuildRoomStatus())
		}
		s.roomMu.RUnlock()
	}

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
