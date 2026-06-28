package server

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/holipay/gametunnel/internal/i18n"
)

// ── checkStatusToken Tests ───────────────────────────────────

func TestCheckStatusToken_NoTokenConfigured(t *testing.T) {
	s := &Server{statusToken: ""}
	r := httptest.NewRequest("GET", "/", nil)
	if !s.checkStatusToken(r) {
		t.Error("should allow access when no token configured")
	}
}

func TestCheckStatusToken_QueryParam(t *testing.T) {
	s := &Server{statusToken: "secret123"}
	r := httptest.NewRequest("GET", "/?token=secret123", nil)
	if !s.checkStatusToken(r) {
		t.Error("valid query param should pass")
	}
}

func TestCheckStatusToken_BearerHeader(t *testing.T) {
	s := &Server{statusToken: "secret123"}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer secret123")
	if !s.checkStatusToken(r) {
		t.Error("valid bearer token should pass")
	}
}

func TestCheckStatusToken_InvalidToken(t *testing.T) {
	s := &Server{statusToken: "secret123"}
	r := httptest.NewRequest("GET", "/?token=wrong", nil)
	if s.checkStatusToken(r) {
		t.Error("wrong token should be rejected")
	}
}

func TestCheckStatusToken_MissingToken(t *testing.T) {
	s := &Server{statusToken: "secret123"}
	r := httptest.NewRequest("GET", "/", nil)
	if s.checkStatusToken(r) {
		t.Error("missing token should be rejected")
	}
}

// ── handleStatusJSON Tests ──────────────────────────────────

func newTestServer() *Server {
	conn, _ := net.ListenUDP("udp", &net.UDPAddr{})
	room, _ := NewRoom(RoomConfig{
		RoomID:     "default",
		Subnet:     &net.IPNet{IP: net.IPv4(10, 10, 0, 0), Mask: net.CIDRMask(24, 32)},
		MaxPlayers: 10,
		Conn:       conn,
	})
	return &Server{
		defaultRoom: room,
		rooms:       map[string]*Room{"default": room},
		startTime:   time.Now(),
		metricsTS:   NewMetricsTimeSeries(),
	}
}

func TestHandleStatusJSON_NoAuth(t *testing.T) {
	s := newTestServer()
	s.statusToken = ""

	req := httptest.NewRequest("GET", "/api/status", nil)
	w := httptest.NewRecorder()
	s.handleStatusJSON(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusOK)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("content-type: got %q", ct)
	}

	var info StatusInfo
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
		t.Fatalf("parse JSON: %v", err)
	}
	if info.Version == "" {
		t.Error("version should not be empty")
	}
	if info.MaxPlayers != 10 {
		t.Errorf("max_players: got %d, want 10", info.MaxPlayers)
	}
}

func TestHandleStatusJSON_WithValidToken(t *testing.T) {
	s := newTestServer()
	s.statusToken = "mytoken"

	req := httptest.NewRequest("GET", "/api/status?token=mytoken", nil)
	w := httptest.NewRecorder()
	s.handleStatusJSON(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusOK)
	}
}

func TestHandleStatusJSON_Forbidden(t *testing.T) {
	s := newTestServer()
	s.statusToken = "mytoken"

	req := httptest.NewRequest("GET", "/api/status", nil)
	w := httptest.NewRecorder()
	s.requireToken(s.handleStatusJSON)(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusForbidden)
	}
}

// ── handleMetricsJSON Tests ─────────────────────────────────

func TestHandleMetricsJSON_NoAuth(t *testing.T) {
	s := newTestServer()
	s.statusToken = ""

	req := httptest.NewRequest("GET", "/api/metrics", nil)
	w := httptest.NewRecorder()
	s.handleMetricsJSON(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusOK)
	}

	var resp MetricsAPIResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse JSON: %v", err)
	}
	if resp.Interval != "1m" {
		t.Errorf("interval: got %q, want %q", resp.Interval, "1m")
	}
	if resp.Window != "1h" {
		t.Errorf("window: got %q, want %q", resp.Window, "1h")
	}
}

func TestHandleMetricsJSON_Forbidden(t *testing.T) {
	s := newTestServer()
	s.statusToken = "secret"

	req := httptest.NewRequest("GET", "/api/metrics", nil)
	w := httptest.NewRecorder()
	s.requireToken(s.handleMetricsJSON)(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusForbidden)
	}
}

// ── handleRoomsJSON Tests ───────────────────────────────────

func TestHandleRoomsJSON_NotEnabled(t *testing.T) {
	s := newTestServer()
	s.statusToken = ""

	req := httptest.NewRequest("GET", "/api/rooms", nil)
	w := httptest.NewRecorder()
	s.handleRoomsJSON(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleRoomsJSON_Forbidden(t *testing.T) {
	s := newTestServer()
	s.statusToken = "secret"
	s.multiRoom = true

	req := httptest.NewRequest("GET", "/api/rooms", nil)
	w := httptest.NewRecorder()
	s.requireToken(s.handleRoomsJSON)(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusForbidden)
	}
}

// ── handleStatusHTML Tests ──────────────────────────────────

func TestHandleStatusHTML_NoAuth(t *testing.T) {
	s := newTestServer()
	s.statusToken = ""
	s.lang = i18n.ZH

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	s.handleStatusHTML(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusOK)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("content-type: got %q", ct)
	}
	if len(w.Body.Bytes()) == 0 {
		t.Error("body should not be empty")
	}
}

func TestHandleStatusHTML_Forbidden(t *testing.T) {
	s := newTestServer()
	s.statusToken = "secret"

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	s.handleStatusHTML(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusForbidden)
	}
}

// ── buildStatusInfo Tests ───────────────────────────────────

func TestBuildStatusInfo_SingleRoom(t *testing.T) {
	s := newTestServer()
	s.version = "1.0.0"

	info := s.buildStatusInfo()
	if info.Version != "1.0.0" {
		t.Errorf("version: got %q, want %q", info.Version, "1.0.0")
	}
	if info.Subnet != "10.10.0.0/24" {
		t.Errorf("subnet: got %q", info.Subnet)
	}
	if info.MultiRoom {
		t.Error("multi_room should be false")
	}
}

func TestBuildStatusInfo_WithPlayer(t *testing.T) {
	s := newTestServer()
	s.version = "1.0.0"

	c := &Client{Username: "test", VirtualIP: net.IPv4(10, 10, 0, 2)}
	c.SetLastSeen(time.Now())
	s.defaultRoom.mu.Lock()
	s.defaultRoom.clients[ipKey(c.VirtualIP)] = c
	s.defaultRoom.mu.Unlock()

	info := s.buildStatusInfo()
	if info.Players != 1 {
		t.Errorf("players: got %d, want 1", info.Players)
	}
	if len(info.Connections) != 1 {
		t.Fatalf("connections: got %d, want 1", len(info.Connections))
	}
	if info.Connections[0].Username != "test" {
		t.Errorf("username: got %q, want %q", info.Connections[0].Username, "test")
	}
}

// ── No CORS wildcard ────────────────────────────────────────

func TestStatusJSON_NoCORS(t *testing.T) {
	s := newTestServer()
	s.statusToken = ""

	req := httptest.NewRequest("GET", "/api/status", nil)
	w := httptest.NewRecorder()
	s.handleStatusJSON(w, req)

	if v := w.Header().Get("Access-Control-Allow-Origin"); v != "" {
		t.Errorf("CORS header should not be set, got %q", v)
	}
}

func TestMetricsJSON_NoCORS(t *testing.T) {
	s := newTestServer()
	s.statusToken = ""

	req := httptest.NewRequest("GET", "/api/metrics", nil)
	w := httptest.NewRecorder()
	s.handleMetricsJSON(w, req)

	if v := w.Header().Get("Access-Control-Allow-Origin"); v != "" {
		t.Errorf("CORS header should not be set, got %q", v)
	}
}
