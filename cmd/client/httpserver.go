package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"time"
)

//go:embed static
var staticFS embed.FS

// HTTPServer serves the control panel and API.
type HTTPServer struct {
	app    *App
	addr   string
	server *http.Server
}

// NewHTTPServer creates a new HTTP server.
func NewHTTPServer(app *App, addr string) *HTTPServer {
	return &HTTPServer{
		app:  app,
		addr: addr,
	}
}

// Start begins serving. Blocks until the server is shut down.
func (s *HTTPServer) Start() error {
	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/connect", s.handleConnect)
	mux.HandleFunc("/api/disconnect", s.handleDisconnect)
	mux.HandleFunc("/api/status", s.handleStatusSSE)

	// Static files (embedded)
	staticRoot, _ := fs.Sub(staticFS, "static")
	mux.Handle("/", http.FileServer(http.FS(staticRoot)))

	s.server = &http.Server{
		Addr:    s.addr,
		Handler: mux,
	}

	log.Printf("[http] 控制面板: http://%s", s.addr)
	return s.server.ListenAndServe()
}

// Stop gracefully shuts down the HTTP server.
func (s *HTTPServer) Stop() {
	if s.server != nil {
		s.server.Shutdown(context.Background())
	}
}

// handleConfig handles GET/POST /api/config
func (s *HTTPServer) handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(s.app.cfg)

	case http.MethodPost:
		var cfg struct {
			ServerAddr string `json:"server_addr"`
			PlayerName string `json:"player_name"`
			RoomID     string `json:"room_id"`
			Password   string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		s.app.cfg.ServerAddr = cfg.ServerAddr
		s.app.cfg.PlayerName = cfg.PlayerName
		s.app.cfg.RoomID = cfg.RoomID
		s.app.cfg.RoomPassword = cfg.Password
		w.Write([]byte(`{"ok":true}`))

	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

// handleConnect handles POST /api/connect
func (s *HTTPServer) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	if s.app.cfg.ServerAddr == "" {
		http.Error(w, `{"error":"server address not configured"}`, http.StatusBadRequest)
		return
	}

	s.app.Connect(s.app.cfg)
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// handleDisconnect handles POST /api/disconnect
func (s *HTTPServer) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	s.app.Disconnect()
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// handleStatusSSE handles GET /api/status (Server-Sent Events)
func (s *HTTPServer) handleStatusSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// Send initial status immediately
	status := s.app.GetStatus()
	fmt.Fprintf(w, "data: %s\n\n", status.JSON())
	flusher.Flush()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			status := s.app.GetStatus()
			fmt.Fprintf(w, "data: %s\n\n", status.JSON())
			flusher.Flush()
		}
	}
}
