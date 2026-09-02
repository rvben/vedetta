package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rvben/vedetta/internal/config"
)

// fakeMQTTOwner stands in for the process-wide client holder. It records the
// configs it is asked to connect with and reports connectivity from a value the
// test can change behind the server's back, which is what a background
// reconnect does in production.
type fakeMQTTOwner struct {
	mu         sync.Mutex
	connected  bool
	replaced   []config.MQTTConfig
	replaceErr error
}

func (f *fakeMQTTOwner) Connected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

func (f *fakeMQTTOwner) Replace(cfg config.MQTTConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replaced = append(f.replaced, cfg)
	if f.replaceErr != nil {
		return f.replaceErr
	}
	f.connected = cfg.Enabled
	return nil
}

func (f *fakeMQTTOwner) setConnected(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected = v
}

func (f *fakeMQTTOwner) calls() []config.MQTTConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]config.MQTTConfig, len(f.replaced))
	copy(out, f.replaced)
	return out
}

var errRefused = errors.New("broker refused the connection")

func mqttSettingsStatus(t *testing.T, srv *Server) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/settings/mqtt", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	status, _ := body["status"].(string)
	return status
}

// The broker can be unreachable when the process starts and connect minutes
// later from the background retry. Nothing calls back into the API server when
// that happens, so the readout has to ask the owner on each request instead of
// caching what it was handed at startup.
func TestMQTTStatusFollowsTheOwnerWithoutBeingTold(t *testing.T) {
	srv, _ := newTestServer(t)
	owner := &fakeMQTTOwner{}
	srv.SetMQTTOwner(owner)
	srv.SetMQTTConfig(config.MQTTConfig{Enabled: true, Host: "192.0.2.10", Port: 1883})
	srv.mqttEnabled = true

	if got := mqttSettingsStatus(t, srv); got != "disconnected" {
		t.Fatalf("before the broker comes up: expected disconnected, got %q", got)
	}

	owner.setConnected(true)

	if got := mqttSettingsStatus(t, srv); got != "connected" {
		t.Fatalf("after the background reconnect: expected connected, got %q", got)
	}

	owner.setConnected(false)

	if got := mqttSettingsStatus(t, srv); got != "disconnected" {
		t.Fatalf("after the broker goes away: expected disconnected, got %q", got)
	}
}

// Saving new broker settings must swap the client every publisher reads, not a
// copy the API server keeps to itself. A server that connected its own client
// here would leave the event loop and the status tickers publishing into the
// connection this handler closed.
func TestUpdateMQTTSettingsSwapsTheSharedClient(t *testing.T) {
	srv, _ := newTestServer(t)
	owner := &fakeMQTTOwner{}
	srv.SetMQTTOwner(owner)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yml")
	initial := "auth:\n  users:\n    - username: admin\n      password_hash: \"$2a$10$7EqJtq98hPqEX7fNZaFWoOHi8V6I5WJFlQ7Y7S6d6n9zQ0jD4S3yu\"\napi:\n  host: 0.0.0.0\n  port: 5050\n  exposure: lan\n"
	if err := os.WriteFile(cfgPath, []byte(initial), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	srv.SetConfigPath(cfgPath)

	payload := `{"enabled":true,"host":"192.0.2.20","port":1883,"username":"test","password":"secret","topic":"vedetta"}`
	req := httptest.NewRequest(http.MethodPut, "/api/settings/mqtt", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	calls := owner.calls()
	if len(calls) != 1 {
		t.Fatalf("expected the shared client to be replaced once, got %d replacements", len(calls))
	}
	if calls[0].Host != "192.0.2.20" || calls[0].Port != 1883 || !calls[0].Enabled {
		t.Fatalf("the shared client was replaced with the wrong settings: %+v", calls[0])
	}
	if got := mqttSettingsStatus(t, srv); got != "connected" {
		t.Fatalf("expected connected after a successful swap, got %q", got)
	}
}

// Turning MQTT off has to reach the shared client too, or the process keeps
// publishing to a broker the operator has disabled.
func TestUpdateMQTTSettingsDisablingSwapsTheSharedClient(t *testing.T) {
	srv, _ := newTestServer(t)
	owner := &fakeMQTTOwner{connected: true}
	srv.SetMQTTOwner(owner)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yml")
	initial := "auth:\n  users:\n    - username: admin\n      password_hash: \"$2a$10$7EqJtq98hPqEX7fNZaFWoOHi8V6I5WJFlQ7Y7S6d6n9zQ0jD4S3yu\"\napi:\n  host: 0.0.0.0\n  port: 5050\n  exposure: lan\n"
	if err := os.WriteFile(cfgPath, []byte(initial), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	srv.SetConfigPath(cfgPath)

	payload := `{"enabled":false,"host":"192.0.2.20","port":1883,"topic":"vedetta"}`
	req := httptest.NewRequest(http.MethodPut, "/api/settings/mqtt", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	calls := owner.calls()
	if len(calls) != 1 {
		t.Fatalf("expected one replacement, got %d", len(calls))
	}
	if calls[0].Enabled {
		t.Fatal("expected the shared client to be told MQTT is off")
	}
	if owner.Connected() {
		t.Fatal("expected the shared client to be closed when MQTT is disabled")
	}
}

// A broker that refuses the new settings leaves MQTT off rather than silently
// keeping the old connection, and the readout says so.
func TestUpdateMQTTSettingsReportsAFailedSwap(t *testing.T) {
	srv, _ := newTestServer(t)
	owner := &fakeMQTTOwner{replaceErr: errRefused}
	srv.SetMQTTOwner(owner)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yml")
	initial := "auth:\n  users:\n    - username: admin\n      password_hash: \"$2a$10$7EqJtq98hPqEX7fNZaFWoOHi8V6I5WJFlQ7Y7S6d6n9zQ0jD4S3yu\"\napi:\n  host: 0.0.0.0\n  port: 5050\n  exposure: lan\n"
	if err := os.WriteFile(cfgPath, []byte(initial), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	srv.SetConfigPath(cfgPath)

	payload := `{"enabled":true,"host":"192.0.2.30","port":1883,"topic":"vedetta"}`
	req := httptest.NewRequest(http.MethodPut, "/api/settings/mqtt", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := mqttSettingsStatus(t, srv); got != "disconnected" {
		t.Fatalf("expected disconnected after a refused broker, got %q", got)
	}
}
