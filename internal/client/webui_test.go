package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebUI_Index(t *testing.T) {
	app := NewApp(&Config{ServerAddr: "1.2.3.4:4700", PlayerName: "test"})
	defer app.Cancel()

	w := NewWebUI(app)
	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()
	w.handleIndex(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "GameTunnel") {
		t.Error("response should contain 'GameTunnel'")
	}
}

func TestWebUI_GetConfig(t *testing.T) {
	app := NewApp(&Config{
		ServerAddr:   "1.2.3.4:4700",
		PlayerName:   "Player1",
		RoomID:       "myroom",
		RoomPassword: "secret",
		Lang:         "zh",
		MTU:          1400,
	})
	defer app.Cancel()

	w := NewWebUI(app)
	req := httptest.NewRequest("GET", "/api/config", nil)
	rr := httptest.NewRecorder()
	w.handleConfig(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}

	var cfg configRequest
	if err := json.Unmarshal(rr.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}
	if cfg.Server != "1.2.3.4:4700" {
		t.Errorf("server: got %q, want %q", cfg.Server, "1.2.3.4:4700")
	}
	if cfg.Name != "Player1" {
		t.Errorf("name: got %q, want %q", cfg.Name, "Player1")
	}
	if cfg.Room != "myroom" {
		t.Errorf("room: got %q, want %q", cfg.Room, "myroom")
	}
	if cfg.Password != "secret" {
		t.Errorf("password: got %q, want %q", cfg.Password, "secret")
	}
	if cfg.MTU != 1400 {
		t.Errorf("mtu: got %d, want %d", cfg.MTU, 1400)
	}
}

func TestWebUI_PostConfig_Validation(t *testing.T) {
	app := NewApp(&Config{})
	defer app.Cancel()

	w := NewWebUI(app)

	// Empty server
	body := `{"server":"","name":"test"}`
	req := httptest.NewRequest("POST", "/api/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	w.handleConfig(rr, req)

	var res map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &res)
	if res["error"] == nil {
		t.Error("expected error for empty server")
	}

	// Empty name
	body = `{"server":"1.2.3.4:4700","name":""}`
	req = httptest.NewRequest("POST", "/api/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	w.handleConfig(rr, req)

	json.Unmarshal(rr.Body.Bytes(), &res)
	if res["error"] == nil {
		t.Error("expected error for empty name")
	}
}

func TestWebUI_PostConfig_Success(t *testing.T) {
	app := NewApp(&Config{ServerAddr: "old:4700"})
	defer app.Cancel()

	w := NewWebUI(app)

	body := `{"server":"1.2.3.4:4700","name":"test","room":"default","mtu":1400}`
	req := httptest.NewRequest("POST", "/api/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	w.handleConfig(rr, req)

	var res map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &res)
	if res["ok"] != true {
		t.Errorf("expected ok=true, got %v", res)
	}
	// Server changed → reconnect should be true
	if res["reconnect"] != true {
		t.Errorf("expected reconnect=true when server changes, got %v", res)
	}

	// Verify config was updated
	app.Mu.RLock()
	if app.Cfg.ServerAddr != "1.2.3.4:4700" {
		t.Errorf("config not updated: server=%q", app.Cfg.ServerAddr)
	}
	app.Mu.RUnlock()
}

func TestWebUI_Status(t *testing.T) {
	app := NewApp(&Config{PlayerName: "test"})
	defer app.Cancel()

	w := NewWebUI(app)
	req := httptest.NewRequest("GET", "/api/status", nil)
	rr := httptest.NewRecorder()
	w.handleStatus(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}

	var status StatusResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}
	if status.PlayerName != "test" {
		t.Errorf("player: got %q, want %q", status.PlayerName, "test")
	}
	if status.Connected {
		t.Error("expected not connected")
	}
}

func TestWebUI_ConnectDisconnect(t *testing.T) {
	app := NewApp(&Config{ServerAddr: "1.2.3.4:4700"})
	defer app.Cancel()

	w := NewWebUI(app)

	// Test disconnect
	req := httptest.NewRequest("POST", "/api/disconnect", nil)
	rr := httptest.NewRecorder()
	w.handleDisconnect(rr, req)

	var res map[string]bool
	json.Unmarshal(rr.Body.Bytes(), &res)
	if res["ok"] != true {
		t.Errorf("disconnect: expected ok=true, got %v", res)
	}

	// Test connect (will fail to connect but should not panic)
	req = httptest.NewRequest("POST", "/api/connect", nil)
	rr = httptest.NewRecorder()
	w.handleConnect(rr, req)

	json.Unmarshal(rr.Body.Bytes(), &res)
	if res["ok"] != true {
		t.Errorf("connect: expected ok=true, got %v", res)
	}
}

func TestWebUI_PostConfig_InvalidAddr(t *testing.T) {
	app := NewApp(&Config{})
	defer app.Cancel()

	w := NewWebUI(app)
	body := `{"server":"","name":"test","room":"default","mtu":1400}`
	req := httptest.NewRequest("POST", "/api/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	w.handleConfig(rr, req)

	var res map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &res)
	if res["error"] == nil {
		t.Error("expected error for empty server address")
	}
}

func TestWebUI_PostConfig_EmptyName(t *testing.T) {
	app := NewApp(&Config{})
	defer app.Cancel()

	w := NewWebUI(app)
	body := `{"server":"1.2.3.4:4700","name":"","room":"default","mtu":1400}`
	req := httptest.NewRequest("POST", "/api/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	w.handleConfig(rr, req)

	var res map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &res)
	if res["error"] == nil {
		t.Error("expected error for empty name")
	}
}
