package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The authed RTSP test endpoint must refuse to dial the cloud-metadata /
// link-local range before probing, so it cannot be abused as an SSRF pivot.
func TestTestRTSPConnection_BlocksLinkLocal(t *testing.T) {
	srv, _ := newTestServer(t)

	body := `{"url":"rtsp://169.254.169.254:554/stream","timeout_seconds":1}`
	req := httptest.NewRequest(http.MethodPost, "/api/cameras/test-rtsp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.TestRTSPConnection(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for blocked target, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ok, _ := resp["ok"].(bool); ok {
		t.Fatalf("expected ok=false, got %v", resp)
	}
	if msg, _ := resp["error"].(string); !strings.Contains(msg, "not allowed") {
		t.Fatalf("expected error to mention 'not allowed', got %q", msg)
	}
}

// The unauthenticated setup-mode RTSP test endpoint shares the SSRF guard.
func TestHandleTestRTSP_BlocksLinkLocal(t *testing.T) {
	db := setupTestDB(t)
	h := NewSetupHandler("/tmp/unused.yml", db, nil)

	body := `{"url":"rtsp://169.254.169.254:554/stream","timeout_seconds":1}`
	req := httptest.NewRequest(http.MethodPost, "/api/setup/test-rtsp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.HandleTestRTSP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for blocked target, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ok, _ := resp["ok"].(bool); ok {
		t.Fatalf("expected ok=false, got %v", resp)
	}
	if msg, _ := resp["error"].(string); !strings.Contains(msg, "not allowed") {
		t.Fatalf("expected error to mention 'not allowed', got %q", msg)
	}
}

// A normal private camera URL passes the guard (it may still fail to connect,
// but it must NOT be rejected as a blocked target).
func TestTestRTSPConnection_AllowsPrivateHost(t *testing.T) {
	srv, _ := newTestServer(t)

	body := `{"url":"rtsp://192.168.1.1:554/stream","timeout_seconds":1}`
	req := httptest.NewRequest(http.MethodPost, "/api/cameras/test-rtsp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.TestRTSPConnection(w, req)

	// The probe will fail to connect (no camera in tests), but that is a 200
	// ok:false dial error, never a 400 policy rejection.
	if w.Code == http.StatusBadRequest {
		t.Fatalf("private host must not be rejected as blocked: %s", w.Body.String())
	}
}
