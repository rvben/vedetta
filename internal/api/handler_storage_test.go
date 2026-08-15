package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/storage"
)

func TestGetStorage_ReturnsBreakdown(t *testing.T) {
	s, db := newTestServer(t)
	now := time.Now().UTC()
	seedSegment(t, db, "cam-a", "/tmp/cam-a/seg-1.mp4",
		now.Add(-time.Hour), now.Add(-30*time.Minute), 1024)

	req := httptest.NewRequest(http.MethodGet, "/api/storage", nil)
	w := httptest.NewRecorder()
	s.GetStorage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["recording"]; !ok {
		t.Errorf("response missing 'recording' key: %v", out)
	}
}

func TestGetStorage_IncludesRecompression(t *testing.T) {
	s, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/storage", nil)
	w := httptest.NewRecorder()
	s.GetStorage(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}

	rc, ok := out["recompression"].(map[string]any)
	if !ok {
		t.Fatalf("response missing 'recompression' object; got %v", out["recompression"])
	}
	for _, key := range []string{
		"enabled", "is_running", "segments_recompressed",
		"clips_recompressed", "bytes_reclaimed", "transcode_failures",
		"worker_crashes", "worker_timeouts",
	} {
		if _, present := rc[key]; !present {
			t.Errorf("recompression missing key %q", key)
		}
	}
	// The test recorder has tiered storage disabled and an idle recompressor.
	if rc["enabled"] != false {
		t.Errorf("enabled=%v, want false (tiered storage off in test)", rc["enabled"])
	}
	if rc["is_running"] != false {
		t.Errorf("is_running=%v, want false for idle recompressor", rc["is_running"])
	}
}

func TestPostStorageDelete_DryRun(t *testing.T) {
	s, db := newTestServer(t)

	camDir := filepath.Join(s.recorder.Path(), "cam-a")
	if err := os.MkdirAll(camDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	paths := []string{}
	for i := 0; i < 3; i++ {
		start := now.AddDate(0, 0, -10-i)
		end := start.Add(time.Hour)
		p := filepath.Join(camDir, fmt.Sprintf("seg-%d.mp4", i))
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		seedSegment(t, db, "cam-a", p, start, end, 1024)
		paths = append(paths, p)
	}

	body := `{"target":"segments","camera":"cam-a","older_than_days":7}`
	req := httptest.NewRequest(http.MethodPost, "/api/storage/delete?dry_run=true",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	dryRun := true
	s.PostStorageDelete(w, req, PostStorageDeleteParams{DryRun: &dryRun})

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var res map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if int(res["segments"].(float64)) != 3 {
		t.Errorf("dry-run reported %v segments, want 3", res["segments"])
	}
	// Files must still exist after a dry-run.
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("dry-run removed %s: %v", p, err)
		}
	}
}

func TestPostStorageDelete_409WhenLockHeld(t *testing.T) {
	s, _ := newTestServer(t)
	s.recorder.LockForTest()
	defer s.recorder.UnlockForTest()

	body := `{"target":"segments","camera":"cam-a","older_than_days":7}`
	req := httptest.NewRequest(http.MethodPost, "/api/storage/delete", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.PostStorageDelete(w, req, PostStorageDeleteParams{})

	if w.Code != http.StatusConflict {
		t.Errorf("status=%d, want 409", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After=%q, want 5", got)
	}
}

func TestPostStorageCleanup_Started(t *testing.T) {
	s, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/storage/cleanup", nil)
	w := httptest.NewRecorder()
	s.PostStorageCleanup(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var res map[string]bool
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res["started"] {
		t.Errorf("started=false, want true")
	}
}

func TestGetStorageAudit_ReturnsEntries(t *testing.T) {
	s, db := newTestServer(t)
	if err := db.InsertStorageAudit(storage.StorageAuditEntry{
		Timestamp: time.Now().UTC(),
		Actor:     "admin",
		ScopeJSON: `{"camera":"cam-a"}`,
		Bytes:     1 << 30,
		Files:     7,
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/storage/audit?limit=10", nil)
	w := httptest.NewRecorder()
	limit := 10
	s.GetStorageAudit(w, req, GetStorageAuditParams{Limit: &limit})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var out []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d entries, want 1", len(out))
	}
	if out[0]["actor"] != "admin" {
		t.Errorf("actor=%v, want admin", out[0]["actor"])
	}
}

func TestPostStorageDelete_RequiresWriteScope(t *testing.T) {
	_, handler, checker := newTestServerWithAuth(t)

	_, rawToken, err := checker.CreateToken("admin", "read-only", []string{"api:read"}, "127.0.0.1")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	body := `{"target":"segments","camera":"cam-a","older_than_days":7}`
	req := httptest.NewRequest(http.MethodPost, "/api/storage/delete", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+rawToken)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("read-only token got status=%d body=%s, want 403", w.Code, w.Body.String())
	}
}

func TestGetStorage_AllowsReadScope(t *testing.T) {
	_, handler, checker := newTestServerWithAuth(t)

	_, rawToken, err := checker.CreateToken("admin", "read-only", []string{"api:read"}, "127.0.0.1")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/storage", nil)
	req.Header.Set("Authorization", "Bearer "+rawToken)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("read token denied: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestStorageDelete_ProtectsOpenSegment(t *testing.T) {
	s, db := newTestServer(t)

	// Write a "live" segment file under the recorder's recording path.
	openDir := filepath.Join(s.recorder.Path(), "cam-a")
	if err := os.MkdirAll(openDir, 0o755); err != nil {
		t.Fatal(err)
	}
	openPath := filepath.Join(openDir, "live.mp4")
	if err := os.WriteFile(openPath, []byte("live"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Register it in the DB as today's segment.
	now := time.Now().UTC()
	seedSegment(t, db, "cam-a", openPath, now, now.Add(time.Hour), 4)

	// Wire a fake open consumer pointing at this path.
	s.recorder.RegisterFakeOpenConsumer("cam-a", openPath)

	body := `{"target":"segments","camera":"cam-a","from":"` +
		now.Format("2006-01-02") + `","to":"` + now.Format("2006-01-02") + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/storage/delete", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.PostStorageDelete(w, req, PostStorageDeleteParams{})

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s, want 422", w.Code, w.Body.String())
	}
	var body422 struct {
		ProtectedPaths []string `json:"protected_paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body422); err != nil {
		t.Fatal(err)
	}
	if len(body422.ProtectedPaths) != 1 || body422.ProtectedPaths[0] != openPath {
		t.Errorf("protected_paths=%v, want [%q]", body422.ProtectedPaths, openPath)
	}
	if _, err := os.Stat(openPath); err != nil {
		t.Errorf("open segment was deleted despite 422: %v", err)
	}
}

func TestStorageDelete_ConcurrentWithCleanup_Returns409(t *testing.T) {
	s, db := newTestServer(t)

	// Seed 5 segments older than 7 days for cam-a, with real files on disk.
	// Lock contention should be detected before any of this matters, but we
	// model a realistic state: a cleanup goroutine is mid-flight while a
	// user-initiated delete arrives.
	camDir := filepath.Join(s.recorder.Path(), "cam-a")
	if err := os.MkdirAll(camDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		start := now.AddDate(0, 0, -10-i)
		end := start.Add(time.Hour)
		p := filepath.Join(camDir, fmt.Sprintf("cleanup-seg-%d.mp4", i))
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		seedSegment(t, db, "cam-a", p, start, end, 1024)
	}

	// Hold the lock as if a periodic cleanup is running.
	s.recorder.LockForTest()
	defer s.recorder.UnlockForTest()

	body := `{"target":"segments","camera":"cam-a","older_than_days":7}`
	req := httptest.NewRequest(http.MethodPost, "/api/storage/delete", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.PostStorageDelete(w, req, PostStorageDeleteParams{})

	if w.Code != http.StatusConflict {
		t.Errorf("status=%d, want 409", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After=%q, want 5", got)
	}
}
