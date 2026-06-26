package client

import (
	_ "embed"
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/holipay/gametunnel/internal/i18n"
)

//go:embed embed/settings.html
var settingsHTML string

// WebUI serves the client settings and status page at localhost.
type WebUI struct {
	app  *App
	srv  *http.Server
	tmpl *template.Template
}

// NewWebUI creates a WebUI for the given App.
func NewWebUI(app *App) *WebUI {
	tmpl := template.Must(template.New("settings").Parse(settingsHTML))
	w := &WebUI{app: app, tmpl: tmpl}

	mux := http.NewServeMux()
	mux.HandleFunc("/", w.handleIndex)
	mux.HandleFunc("/api/config", w.handleConfig)
	mux.HandleFunc("/api/status", w.handleStatus)
	mux.HandleFunc("/api/connect", w.handleConnect)
	mux.HandleFunc("/api/disconnect", w.handleDisconnect)

	w.srv = &http.Server{Handler: mux}
	return w
}

// Start begins listening on addr (e.g. "127.0.0.1:4702").
func (w *WebUI) Start(addr string) {
	go func() {
		log.Printf("[webui] listening on http://%s", addr)
		if err := w.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[webui] server error: %v", err)
		}
	}()
}

// Stop gracefully shuts down the WebUI server.
func (w *WebUI) Stop() {
	w.srv.Close()
}

func (w *WebUI) handleIndex(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	t := i18n.T()
	if err := w.tmpl.Execute(rw, struct{ T *i18n.Strings }{T: t}); err != nil {
		log.Printf("[webui] template error: %v", err)
	}
}

func (w *WebUI) handleStatus(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(w.app.GetStatus())
}

func (w *WebUI) handleConnect(rw http.ResponseWriter, r *http.Request) {
	w.app.Mu.RLock()
	cfg := w.app.Cfg
	w.app.Mu.RUnlock()
	w.app.Connect(cfg)
	writeJSON(rw, map[string]bool{"ok": true})
}

func (w *WebUI) handleDisconnect(rw http.ResponseWriter, r *http.Request) {
	w.app.Disconnect()
	writeJSON(rw, map[string]bool{"ok": true})
}

type configRequest struct {
	Server   string `json:"server"`
	Name     string `json:"name"`
	Room     string `json:"room"`
	Password string `json:"password"`
	Lang     string `json:"lang"`
	MTU      int    `json:"mtu"`
}

func (w *WebUI) handleConfig(rw http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		w.app.Mu.RLock()
		cfg := w.app.Cfg
		w.app.Mu.RUnlock()
		writeJSON(rw, configRequest{
			Server:   cfg.ServerAddr,
			Name:     cfg.PlayerName,
			Room:     cfg.RoomID,
			Password: cfg.RoomPassword,
			Lang:     cfg.Lang,
			MTU:      cfg.MTU,
		})
		return
	}

	if r.Method != "POST" {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req configRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(rw, map[string]interface{}{"ok": false, "error": "invalid JSON"})
		return
	}

	// Validate
	req.Server = strings.TrimSpace(req.Server)
	req.Name = strings.TrimSpace(req.Name)
	req.Room = strings.TrimSpace(req.Room)

	if req.Server == "" {
		writeJSON(rw, map[string]interface{}{"ok": false, "error": i18n.T().WebUIInvalidAddr})
		return
	}
	if req.Name == "" {
		writeJSON(rw, map[string]interface{}{"ok": false, "error": i18n.T().WebUINameEmpty})
		return
	}
	if req.Room == "" {
		req.Room = "default"
	}
	if req.MTU < 576 || req.MTU > 9000 {
		req.MTU = 1400
	}
	if req.Lang != "zh" && req.Lang != "en" {
		req.Lang = "zh"
	}

	cfg := &Config{
		ServerAddr:   req.Server,
		PlayerName:   req.Name,
		RoomID:       req.Room,
		RoomPassword: req.Password,
		Lang:         req.Lang,
		MTU:          req.MTU,
	}

	// Save to disk
	if err := SaveConfig(cfg); err != nil {
		log.Printf("[webui] save config: %v", err)
	}

	// Check if server address changed
	w.app.Mu.RLock()
	oldServer := w.app.Cfg.ServerAddr
	w.app.Mu.Unlock()

	serverChanged := oldServer != cfg.ServerAddr

	// Update running config
	w.app.Mu.Lock()
	w.app.Cfg = cfg
	w.app.Mu.Unlock()

	// Update language if changed
	if cfg.Lang != "" {
		i18n.Set(i18n.ParseLang(cfg.Lang))
	}

	reconnect := serverChanged
	if serverChanged {
		w.app.Disconnect()
		w.app.Connect(cfg)
	}

	writeJSON(rw, map[string]interface{}{
		"ok":        true,
		"reconnect": reconnect,
	})
}

func writeJSON(rw http.ResponseWriter, v interface{}) {
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(v)
}

func init() {
	// Ensure MTU is handled as string in form, parsed to int
	_ = strconv.Itoa // used in template
}
