package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/storage"
)

func setupTestDB(t *testing.T) *storage.DB {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSetupHandler_CreateAccount(t *testing.T) {
	db := setupTestDB(t)
	configPath := filepath.Join(t.TempDir(), "config.yml")
	done := make(chan struct{})
	h := NewSetupHandler(configPath, db, done)

	body := `{"username":"admin","password":"testpass123"}`
	req := httptest.NewRequest(http.MethodPost, "/api/setup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.HandleSetup(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("expected status ok, got %q", resp["status"])
	}

	// Config file should exist
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Error("config file was not written")
	}

	// User should be in DB
	users, err := db.ListAuthUsers()
	if err != nil {
		t.Fatalf("ListAuthUsers: %v", err)
	}
	if len(users) == 0 {
		t.Fatal("expected at least one auth user in DB")
	}
	if users[0].Username != "admin" {
		t.Errorf("expected username admin, got %q", users[0].Username)
	}
}

func TestSetupHandler_CreateAccount_MissingFields(t *testing.T) {
	db := setupTestDB(t)
	done := make(chan struct{})
	h := NewSetupHandler("/tmp/unused.yml", db, done)

	tests := []struct {
		name string
		body string
	}{
		{"empty username", `{"username":"","password":"pass"}`},
		{"empty password", `{"username":"admin","password":""}`},
		{"both empty", `{"username":"","password":""}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/setup", strings.NewReader(tt.body))
			w := httptest.NewRecorder()
			h.HandleSetup(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d", w.Code)
			}
		})
	}
}

func TestSetupHandler_ReadOnly(t *testing.T) {
	db := setupTestDB(t)
	// Use a path inside a non-existent directory to simulate read-only
	configPath := filepath.Join(t.TempDir(), "readonly", "subdir", "config.yml")
	done := make(chan struct{})
	h := NewSetupHandler(configPath, db, done)

	body := `{"username":"admin","password":"testpass123"}`
	req := httptest.NewRequest(http.MethodPost, "/api/setup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.HandleSetup(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp["status"] != "config_readonly" {
		t.Errorf("expected status config_readonly, got %q", resp["status"])
	}
	if _, ok := resp["config_yaml"]; !ok {
		t.Error("expected config_yaml in response")
	}

	// User should still be in DB even though config write failed
	users, err := db.ListAuthUsers()
	if err != nil {
		t.Fatalf("ListAuthUsers: %v", err)
	}
	if len(users) == 0 {
		t.Fatal("expected auth user in DB despite config write failure")
	}
}

func TestSetupHandler_Discover(t *testing.T) {
	db := setupTestDB(t)
	done := make(chan struct{})
	h := NewSetupHandler("/tmp/unused.yml", db, done)

	req := httptest.NewRequest(http.MethodGet, "/api/discover", nil)
	w := httptest.NewRecorder()

	h.HandleDiscover(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if _, ok := resp["cameras"]; !ok {
		t.Error("expected cameras key in response")
	}
}

func TestSetupHandler_AddCameras(t *testing.T) {
	db := setupTestDB(t)
	configPath := filepath.Join(t.TempDir(), "config.yml")
	done := make(chan struct{})
	h := NewSetupHandler(configPath, db, done)

	// Write initial config so AppendCamera has a file to work with
	if err := os.WriteFile(configPath, []byte("auth:\n  users: []\n"), 0600); err != nil {
		t.Fatalf("write initial config: %v", err)
	}

	body := `{"cameras":[{"name":"front_door","url":"rtsp://admin:pass@192.168.1.100:554/stream1","record_url":"rtsp://admin:pass@192.168.1.100:554/stream1"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/cameras", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.HandleAddCameras(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("expected status ok, got %q", resp["status"])
	}

	// Config file should contain the camera
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(data), "front_door") {
		t.Error("config does not contain camera name")
	}

	// setupDone should be signaled
	select {
	case <-done:
		// ok
	default:
		t.Error("setupDone channel was not closed")
	}
}

func TestSetupHandler_Complete(t *testing.T) {
	db := setupTestDB(t)
	if err := db.SaveAuthUser("admin", "hash"); err != nil {
		t.Fatalf("SaveAuthUser: %v", err)
	}
	done := make(chan struct{})
	h := NewSetupHandler("/tmp/unused.yml", db, done)

	req := httptest.NewRequest(http.MethodPost, "/api/setup/complete", nil)
	w := httptest.NewRecorder()

	h.HandleComplete(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	select {
	case <-done:
		// ok
	default:
		t.Error("setupDone channel was not closed")
	}

	// Calling again should not panic
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/setup/complete", nil)
	h.HandleComplete(w2, req2)
	if w2.Code != http.StatusOK {
		t.Errorf("second call: expected 200, got %d", w2.Code)
	}
}

func TestSetupHandler_Thumbnail_NotFound(t *testing.T) {
	db := setupTestDB(t)
	done := make(chan struct{})
	h := NewSetupHandler("/tmp/unused.yml", db, done)

	req := httptest.NewRequest(http.MethodGet, "/api/discover/thumbnail/192.168.1.1", nil)
	req.SetPathValue("ip", "192.168.1.1")
	w := httptest.NewRecorder()

	h.HandleThumbnail(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestSetupHandler_Thumbnail_Found(t *testing.T) {
	db := setupTestDB(t)
	done := make(chan struct{})
	h := NewSetupHandler("/tmp/unused.yml", db, done)

	// Pre-populate thumbnail
	h.mu.Lock()
	h.thumbnails["192.168.1.1"] = []byte{0xFF, 0xD8, 0xFF, 0xE0} // JPEG magic bytes
	h.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/discover/thumbnail/192.168.1.1", nil)
	req.SetPathValue("ip", "192.168.1.1")
	w := httptest.NewRecorder()

	h.HandleThumbnail(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("expected content-type image/jpeg, got %q", ct)
	}
	if len(w.Body.Bytes()) != 4 {
		t.Errorf("expected 4 bytes, got %d", len(w.Body.Bytes()))
	}
}

func TestSetupFlow_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yml")
	dbPath := filepath.Join(dir, "test.db")

	db, err := storage.New(dbPath)
	if err != nil {
		t.Fatalf("DB: %v", err)
	}
	defer db.Close()

	setupDone := make(chan struct{})
	server := NewSetupMode(config.APIConfig{Host: "127.0.0.1", Port: 0}, db, configPath, setupDone)

	// Start on random port using httptest
	ts := httptest.NewServer(server.mux)
	defer ts.Close()

	// The wizard carries the one-time setup code printed at startup.
	code := server.setupHandler.setupCode()
	do := func(method, path, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatalf("NewRequest %s %s: %v", method, path, err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set(setupCodeHeader, code)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp
	}

	// 1. Non-setup API routes should be blocked (403)
	resp, _ := http.Get(ts.URL + "/api/events")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for /api/events during setup, got %d", resp.StatusCode)
	}

	// 2. Setup status should indicate setup mode
	resp, _ = http.Get(ts.URL + "/api/setup/status")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for setup status, got %d", resp.StatusCode)
	}

	// 3. Create admin account
	resp = do(http.MethodPost, "/api/setup", `{"username":"admin","password":"test1234"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup failed: %d", resp.StatusCode)
	}

	// Verify session cookies are set (auto-login)
	var hasSession, hasCSRF bool
	for _, c := range resp.Cookies() {
		if c.Name == "vedetta_session" && c.Value != "" {
			hasSession = true
		}
		if c.Name == "vedetta_csrf" && c.Value != "" {
			hasCSRF = true
		}
	}
	if !hasSession || !hasCSRF {
		t.Error("expected session cookies after account creation")
	}

	// Verify config file was created
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("config file not created: %v", err)
	}

	// 4. Discovery should work (returns 200, may have empty cameras)
	resp = do(http.MethodGet, "/api/discover", "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("discover should return 200, got %d", resp.StatusCode)
	}

	// 5. Signal complete (skip cameras)
	resp = do(http.MethodPost, "/api/setup/complete", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("complete failed: %d", resp.StatusCode)
	}

	// 6. setupDone should be closed
	select {
	case <-setupDone:
	case <-time.After(2 * time.Second):
		t.Fatal("setupDone not signaled within 2s")
	}

	// 7. Config should be loadable with valid auth
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config not loadable: %v", err)
	}
	if len(cfg.Auth.Users) != 1 {
		t.Errorf("expected 1 auth user, got %d", len(cfg.Auth.Users))
	}

	// 8. Auth user should be in DB
	users, _ := db.ListAuthUsers()
	if len(users) != 1 || users[0].Username != "admin" {
		t.Errorf("expected admin in DB, got %v", users)
	}
}
