package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/recording"
	"github.com/rvben/vedetta/internal/storage"
)

// newTestServer creates a Server backed by an in-memory SQLite database
// and an empty camera manager (no real cameras).
func newTestServer(t *testing.T) (*Server, *storage.DB) {
	t.Helper()

	db, err := storage.New(":memory:")
	if err != nil {
		t.Fatalf("create in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mgr := camera.NewManager(nil, nil, config.MotionConfig{}, nil, nil, nil, nil, "", 85, "", nil, nil, "", nil)
	rec := recording.New(config.RecordingConfig{
		Path: t.TempDir(),
	}, config.EventConfig{RetainDays: 90}, db, nil, "")

	apiCfg := config.APIConfig{Host: "127.0.0.1", Port: 0}
	srv := New(apiCfg, nil, db)
	srv.SetSubsystems(mgr, rec, nil, nil, nil, "", "", nil, nil)
	return srv, db
}

// seedEvent inserts a test event into the database.
func seedEvent(t *testing.T, db *storage.DB, id, cameraName, label string, score float32, ts time.Time) {
	t.Helper()
	err := db.SaveEvent(camera.Event{
		ID:         id,
		CameraName: cameraName,
		Label:      label,
		Score:      score,
		Box:        [4]int{10, 20, 100, 200},
		Timestamp:  ts,
	})
	if err != nil {
		t.Fatalf("seed event %s: %v", id, err)
	}
}

// seedSegment inserts a test segment into the database.
func seedSegment(t *testing.T, db *storage.DB, cam, path string, start, end time.Time, size int64) {
	t.Helper()
	err := db.SaveSegment(storage.SegmentRecord{
		Camera:    cam,
		Path:      path,
		StartTime: start,
		EndTime:   end,
		SizeBytes: size,
	})
	if err != nil {
		t.Fatalf("seed segment %s: %v", path, err)
	}
}

// --- Server lifecycle tests ---

func TestServerStartAndShutdown(t *testing.T) {
	srv, _ := newTestServer(t)

	// Override config to use a random port
	srv.config.Port = 0
	srv.config.Host = "127.0.0.1"

	// Start in background
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start()
	}()

	// Give server time to bind
	time.Sleep(50 * time.Millisecond)

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}

	// Start should have returned http.ErrServerClosed
	err := <-errCh
	if err != nil && strings.Contains(err.Error(), "operation not permitted") {
		t.Skipf("listener unavailable in this environment: %v", err)
	}
	if !errors.Is(err, http.ErrServerClosed) {
		t.Errorf("Start() returned %v, want http.ErrServerClosed", err)
	}
}

func TestServerShutdownWithoutStart(t *testing.T) {
	srv, _ := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown() on never-started server should return nil, got %v", err)
	}
}

// --- Helper function unit tests ---

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		input int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{1073741824, "1.0 GB"},
		{1099511627776, "1.0 TB"},
		{2199023255552, "2.0 TB"},
	}
	for _, tt := range tests {
		got := formatBytes(tt.input)
		if got != tt.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestDisplayName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"front_door", "Front Door"},
		{"back_yard", "Back Yard"},
		{"garage", "Garage"},
		{"", ""},
		{"a_b_c", "A B C"},
		{"already", "Already"},
	}
	for _, tt := range tests {
		got := displayName(tt.input)
		if got != tt.want {
			t.Errorf("displayName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		input time.Duration
		want  string
	}{
		{0, "0s"},
		{30 * time.Second, "30s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m"},
		{90 * time.Second, "1m"},
		{5 * time.Minute, "5m"},
		{59 * time.Minute, "59m"},
		{60 * time.Minute, "1h"},
		{90 * time.Minute, "1h30m"},
		{2*time.Hour + 15*time.Minute, "2h15m"},
		{24 * time.Hour, "24h"},
	}
	for _, tt := range tests {
		got := formatDuration(tt.input)
		if got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// --- JSON API handler tests ---

func TestHandleHealth(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/health: status = %d, want %d", w.Code, http.StatusOK)
	}

	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %q, want %q", body["status"], "ok")
	}
	if _, ok := body["checks"]; !ok {
		t.Error("response missing 'checks' key")
	}
	if body["version"] != "0.1.0" {
		t.Errorf("version = %v, want %q", body["version"], "0.1.0")
	}

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}
}

func TestHandleListCameras_Empty(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/cameras", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/cameras: status = %d, want %d", w.Code, http.StatusOK)
	}

	var body []map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body) != 0 {
		t.Errorf("cameras = %v, want empty", body)
	}
}

func TestHandleListEvents_NoEvents(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/events: status = %d, want %d", w.Code, http.StatusOK)
	}

	var body map[string]json.RawMessage
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, ok := body["events"]; !ok {
		t.Error("response missing 'events' key")
	}
}

func TestHandleListEvents_WithData(t *testing.T) {
	srv, db := newTestServer(t)
	now := time.Now().UTC().Truncate(time.Second)

	seedEvent(t, db, "evt-1", "front_door", "person", 0.95, now)
	seedEvent(t, db, "evt-2", "back_yard", "car", 0.80, now.Add(-time.Minute))
	seedEvent(t, db, "evt-3", "front_door", "dog", 0.70, now.Add(-2*time.Minute))

	t.Run("unfiltered returns all", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		var body struct {
			Events []camera.Event `json:"events"`
		}
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(body.Events) != 3 {
			t.Errorf("got %d events, want 3", len(body.Events))
		}
	})

	t.Run("filter by camera", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/events?camera=front_door", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		var body struct {
			Events []camera.Event `json:"events"`
		}
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(body.Events) != 2 {
			t.Errorf("got %d events, want 2", len(body.Events))
		}
		for _, e := range body.Events {
			if e.CameraName != "front_door" {
				t.Errorf("event camera = %q, want %q", e.CameraName, "front_door")
			}
		}
	})

	t.Run("filter by label", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/events?label=car", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		var body struct {
			Events []camera.Event `json:"events"`
		}
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(body.Events) != 1 {
			t.Errorf("got %d events, want 1", len(body.Events))
		}
	})

	t.Run("limit", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/events?limit=2", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		var body struct {
			Events []camera.Event `json:"events"`
		}
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(body.Events) != 2 {
			t.Errorf("got %d events, want 2", len(body.Events))
		}
	})
}

func TestHandleGetEvent(t *testing.T) {
	srv, db := newTestServer(t)
	now := time.Now().UTC().Truncate(time.Second)
	seedEvent(t, db, "evt-42", "garage", "person", 0.99, now)

	t.Run("found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/events/evt-42", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}

		var event camera.Event
		if err := json.NewDecoder(w.Body).Decode(&event); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if event.ID != "evt-42" {
			t.Errorf("event ID = %q, want %q", event.ID, "evt-42")
		}
		if event.Label != "person" {
			t.Errorf("event label = %q, want %q", event.Label, "person")
		}
	})

	t.Run("not found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/events/nonexistent", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
		}

		var body map[string]string
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body["error"] != "event not found" {
			t.Errorf("error = %q, want %q", body["error"], "event not found")
		}
	})
}

func TestHandleEventSnapshot_NotFound(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/events/no-such-id/snapshot", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleEventClip_NotFound(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/events/no-such-id/clip", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleEventSnapshot_NoPath(t *testing.T) {
	srv, db := newTestServer(t)
	now := time.Now().UTC().Truncate(time.Second)
	// Event exists but has no snapshot_path
	seedEvent(t, db, "evt-nosnap", "cam1", "person", 0.9, now)

	req := httptest.NewRequest(http.MethodGet, "/api/events/evt-nosnap/snapshot", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleEventClip_NoPath(t *testing.T) {
	srv, db := newTestServer(t)
	now := time.Now().UTC().Truncate(time.Second)
	// Event exists but has no clip_path
	seedEvent(t, db, "evt-noclip", "cam1", "person", 0.9, now)

	req := httptest.NewRequest(http.MethodGet, "/api/events/evt-noclip/clip", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleSnapshot_CameraNotFound(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/cameras/nonexistent/snapshot", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}

	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["error"] != "camera not found" {
		t.Errorf("error = %q, want %q", body["error"], "camera not found")
	}
}

func TestHandleSystemAPI(t *testing.T) {
	srv, db := newTestServer(t)

	// Seed some segments to have storage data
	now := time.Now().UTC()
	seedSegment(t, db, "cam1", "/tmp/seg1.mp4", now.Add(-10*time.Minute), now, 1048576)
	srv.recorder.RefreshStats()

	req := httptest.NewRequest(http.MethodGet, "/api/system", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if body["version"] != "0.1.0" {
		t.Errorf("version = %v, want %q", body["version"], "0.1.0")
	}
	if body["decoder"] != "native Go" {
		t.Errorf("decoder = %v, want %q", body["decoder"], "native Go")
	}
	if body["cameras"].(float64) != 0 {
		t.Errorf("cameras = %v, want 0", body["cameras"])
	}
	if _, ok := body["uptime"]; !ok {
		t.Error("response missing 'uptime' key")
	}
	if _, ok := body["storage"]; !ok {
		t.Error("response missing 'storage' key")
	}
	storageBytes := body["storage_bytes"].(float64)
	if storageBytes != 1048576 {
		t.Errorf("storage_bytes = %v, want 1048576", storageBytes)
	}
}

// --- HTML partial handler tests ---

func TestHandleDashboardStatsPartial(t *testing.T) {
	srv, db := newTestServer(t)
	now := time.Now().UTC()
	seedEvent(t, db, "today-1", "cam1", "person", 0.9, now)
	seedSegment(t, db, "cam1", "/tmp/s1.mp4", now.Add(-5*time.Minute), now, 2048)
	srv.recorder.RefreshStats()

	req := httptest.NewRequest(http.MethodGet, "/partials/dashboard-stats", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	ct := w.Header().Get("Content-Type")
	if ct != "text/html" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/html")
	}

	body := w.Body.String()
	if len(body) == 0 {
		t.Error("response body is empty")
	}
	// The dashboard stats template includes stat-card divs
	if !contains(body, "stat-card") {
		t.Error("response missing stat-card class")
	}
	if !contains(body, "Cameras") {
		t.Error("response missing Cameras label")
	}
	if !contains(body, "Events Today") {
		t.Error("response missing Events Today label")
	}
	if !contains(body, "Storage") {
		t.Error("response missing Storage label")
	}
}

func TestHandleCameraGridPartial_Empty(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/partials/camera-grid", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	ct := w.Header().Get("Content-Type")
	if ct != "text/html" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/html")
	}
	// No cameras renders an empty-state hero with discover/add CTAs.
	body := w.Body.String()
	if !strings.Contains(body, "No cameras configured yet") {
		t.Errorf("expected empty-state hero in body, got %q", body)
	}
}

func TestHandleEventsGalleryPartial_Empty(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/partials/events-gallery", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	body := w.Body.String()
	if !contains(body, "No events recorded yet") {
		t.Errorf("expected empty state message, got %q", body)
	}
}

func TestHandleEventsGalleryPartial_WithEvents(t *testing.T) {
	srv, db := newTestServer(t)
	now := time.Now().UTC().Truncate(time.Second)
	seedEvent(t, db, "gal-1", "front_door", "person", 0.85, now)

	req := httptest.NewRequest(http.MethodGet, "/partials/events-gallery", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	body := w.Body.String()
	if !contains(body, "event-card") {
		t.Error("response missing event-card class")
	}
	if !contains(body, "person") {
		t.Error("response missing label 'person'")
	}
	if !contains(body, "front_door") {
		t.Error("response missing camera name 'front_door'")
	}
}

func TestHandleEventsGalleryPartial_Filters(t *testing.T) {
	srv, db := newTestServer(t)
	now := time.Now().UTC().Truncate(time.Second)
	seedEvent(t, db, "f-1", "cam_a", "person", 0.9, now)
	seedEvent(t, db, "f-2", "cam_b", "car", 0.8, now)

	req := httptest.NewRequest(http.MethodGet, "/partials/events-gallery?camera=cam_a", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !contains(body, "cam_a") {
		t.Error("filtered response should contain cam_a")
	}
	if contains(body, "cam_b") {
		t.Error("filtered response should not contain cam_b")
	}
}

func TestHandleEventDetailPartial(t *testing.T) {
	srv, db := newTestServer(t)
	now := time.Now().UTC().Truncate(time.Second)
	seedEvent(t, db, "detail-1", "garage", "cat", 0.77, now)

	t.Run("found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/partials/event/detail-1", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		body := w.Body.String()
		if !contains(body, "cat") {
			t.Error("response missing label 'cat'")
		}
		if !contains(body, "garage") {
			t.Error("response missing camera 'garage'")
		}
		if !contains(body, "detail-1") {
			t.Error("response missing event ID")
		}
	})

	t.Run("not found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/partials/event/nonexistent", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
		}
	})
}

func TestHandleSystemStatusPartial(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/partials/system-status", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	ct := w.Header().Get("Content-Type")
	if ct != "text/html" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/html")
	}
	body := w.Body.String()
	if !contains(body, "cameras") {
		t.Error("response missing 'cameras' text")
	}
	if !contains(body, "online") {
		t.Error("response missing 'online' text")
	}
}

func TestHandleSystemPartial(t *testing.T) {
	srv, db := newTestServer(t)
	now := time.Now().UTC()
	seedSegment(t, db, "cam1", "/tmp/sys-seg.mp4", now.Add(-5*time.Minute), now, 5242880)
	srv.recorder.RefreshStats()

	req := httptest.NewRequest(http.MethodGet, "/partials/system", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	ct := w.Header().Get("Content-Type")
	if ct != "text/html" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/html")
	}
	body := w.Body.String()
	if !contains(body, "System Info") {
		t.Error("response missing 'System Info' header")
	}
	if !contains(body, "0.1.0") {
		t.Error("response missing version")
	}
	if !contains(body, "Storage") {
		t.Error("response missing 'Storage' section")
	}
	if !contains(body, "5.0 MB") {
		t.Errorf("response missing formatted storage value '5.0 MB', body: %s", body)
	}
}

func TestHandleRecordingsSummary_Empty(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/recordings/summary?date=2026-03-23", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	cameras, ok := body["cameras"].([]any)
	if !ok {
		t.Fatalf("cameras field missing or wrong type")
	}
	if len(cameras) != 0 {
		t.Errorf("cameras = %v, want empty", cameras)
	}
}

func TestHandleRecordingsSummary_WithData(t *testing.T) {
	srv, db := newTestServer(t)

	now := time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC)
	seedSegment(t, db, "cam1", "/seg1.mp4", now, now.Add(10*time.Minute), 1024*1024)
	seedSegment(t, db, "cam1", "/seg2.mp4", now.Add(10*time.Minute), now.Add(20*time.Minute), 2*1024*1024)

	req := httptest.NewRequest(http.MethodGet, "/api/recordings/summary?date=2026-03-23", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	cameras := body["cameras"].([]any)
	if len(cameras) != 1 {
		t.Fatalf("cameras count = %d, want 1", len(cameras))
	}
	cam := cameras[0].(map[string]any)
	if cam["name"] != "cam1" {
		t.Errorf("camera name = %v, want cam1", cam["name"])
	}
	segs := cam["segments"].([]any)
	if len(segs) != 2 {
		t.Errorf("segments count = %d, want 2", len(segs))
	}
	totalBytes := body["total_bytes"].(float64)
	if totalBytes != 3*1024*1024 {
		t.Errorf("total_bytes = %v, want %v", totalBytes, 3*1024*1024)
	}
}

// --- Edge case tests ---

func TestHandleListEvents_InvalidLimit(t *testing.T) {
	srv, db := newTestServer(t)
	now := time.Now().UTC().Truncate(time.Second)
	seedEvent(t, db, "lim-1", "cam1", "person", 0.9, now)

	// Invalid limit should fall back to default (50)
	req := httptest.NewRequest(http.MethodGet, "/api/events?limit=abc", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var body struct {
		Events []camera.Event `json:"events"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Events) != 1 {
		t.Errorf("got %d events, want 1", len(body.Events))
	}
}

func TestHandleListEvents_ZeroLimit(t *testing.T) {
	srv, db := newTestServer(t)
	now := time.Now().UTC().Truncate(time.Second)
	seedEvent(t, db, "z-1", "cam1", "person", 0.9, now)

	// Zero limit should fall back to default (50) since parsed <= 0
	req := httptest.NewRequest(http.MethodGet, "/api/events?limit=0", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestHandleListEvents_NegativeLimit(t *testing.T) {
	srv, db := newTestServer(t)
	now := time.Now().UTC().Truncate(time.Second)
	seedEvent(t, db, "neg-1", "cam1", "person", 0.9, now)

	req := httptest.NewRequest(http.MethodGet, "/api/events?limit=-5", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestWriteJSON_ContentType(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, http.StatusCreated, map[string]string{"key": "value"})

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", w.Code, http.StatusCreated)
	}
	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}
}

func TestHandleListEvents_CombinedFilters(t *testing.T) {
	srv, db := newTestServer(t)
	now := time.Now().UTC().Truncate(time.Second)

	seedEvent(t, db, "cf-1", "front_door", "person", 0.9, now)
	seedEvent(t, db, "cf-2", "front_door", "car", 0.8, now.Add(-time.Minute))
	seedEvent(t, db, "cf-3", "back_yard", "person", 0.7, now.Add(-2*time.Minute))

	req := httptest.NewRequest(http.MethodGet, "/api/events?camera=front_door&label=person", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	var body struct {
		Events []camera.Event `json:"events"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Events) != 1 {
		t.Errorf("got %d events, want 1", len(body.Events))
	}
	if len(body.Events) > 0 && body.Events[0].ID != "cf-1" {
		t.Errorf("event ID = %q, want %q", body.Events[0].ID, "cf-1")
	}
}

func TestHandleListEvents_OrderedByTimestampDesc(t *testing.T) {
	srv, db := newTestServer(t)
	now := time.Now().UTC().Truncate(time.Second)

	seedEvent(t, db, "ord-old", "cam1", "person", 0.9, now.Add(-10*time.Minute))
	seedEvent(t, db, "ord-new", "cam1", "person", 0.8, now)
	seedEvent(t, db, "ord-mid", "cam1", "person", 0.7, now.Add(-5*time.Minute))

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	var body struct {
		Events []camera.Event `json:"events"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Events) != 3 {
		t.Fatalf("got %d events, want 3", len(body.Events))
	}
	if body.Events[0].ID != "ord-new" {
		t.Errorf("first event = %q, want %q (most recent)", body.Events[0].ID, "ord-new")
	}
	if body.Events[1].ID != "ord-mid" {
		t.Errorf("second event = %q, want %q", body.Events[1].ID, "ord-mid")
	}
	if body.Events[2].ID != "ord-old" {
		t.Errorf("third event = %q, want %q (oldest)", body.Events[2].ID, "ord-old")
	}
}

// --- Camera Timeline tests ---

func TestHandleCameraTimeline_CameraNotFound(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/cameras/nonexistent/timeline?date=2025-01-15", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}

	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["error"] != "camera not found" {
		t.Errorf("error = %q, want %q", body["error"], "camera not found")
	}
}

// --- Recordings Calendar tests ---

func TestHandleRecordingsCalendar_WithSegments(t *testing.T) {
	srv, db := newTestServer(t)

	// Seed segments on specific days in January 2025
	day5 := time.Date(2025, 1, 5, 10, 0, 0, 0, time.UTC)
	day10 := time.Date(2025, 1, 10, 14, 0, 0, 0, time.UTC)
	day20 := time.Date(2025, 1, 20, 8, 0, 0, 0, time.UTC)

	seedSegment(t, db, "cam1", "/tmp/cal-seg1.mp4", day5, day5.Add(time.Hour), 1024)
	seedSegment(t, db, "cam1", "/tmp/cal-seg2.mp4", day10, day10.Add(time.Hour), 2048)
	seedSegment(t, db, "cam2", "/tmp/cal-seg3.mp4", day20, day20.Add(time.Hour), 4096)

	t.Run("no camera filter returns days across all cameras", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/recordings/calendar?month=2025-01", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}

		var body struct {
			Days []int `json:"days"`
		}
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(body.Days) != 3 {
			t.Fatalf("got %d days, want 3: %v", len(body.Days), body.Days)
		}
		want := []int{5, 10, 20}
		for i, d := range want {
			if body.Days[i] != d {
				t.Errorf("days[%d] = %d, want %d", i, body.Days[i], d)
			}
		}
	})

	t.Run("filter by specific camera", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/recordings/calendar?month=2025-01&camera=cam1", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}

		var body struct {
			Days []int `json:"days"`
		}
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(body.Days) != 2 {
			t.Fatalf("got %d days, want 2: %v", len(body.Days), body.Days)
		}
		if body.Days[0] != 5 || body.Days[1] != 10 {
			t.Errorf("days = %v, want [5, 10]", body.Days)
		}
	})
}

func TestHandleRecordingsCalendar_EmptyMonth(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/recordings/calendar?month=2030-06", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var body struct {
		Days []int `json:"days"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Days) != 0 {
		t.Errorf("got %d days, want 0: %v", len(body.Days), body.Days)
	}
}

func TestHandleRecordingsCalendar_DefaultsToCurrentMonth(t *testing.T) {
	srv, db := newTestServer(t)

	// Seed a segment for today
	now := time.Now().UTC()
	seedSegment(t, db, "cam1", "/tmp/cal-today.mp4", now.Add(-time.Hour), now, 1024)

	req := httptest.NewRequest(http.MethodGet, "/api/recordings/calendar", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var body struct {
		Days []int `json:"days"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Days) != 1 {
		t.Fatalf("got %d days, want 1: %v", len(body.Days), body.Days)
	}
	if body.Days[0] != now.Day() {
		t.Errorf("day = %d, want %d (today)", body.Days[0], now.Day())
	}
}

// --- Event Counts tests ---

func TestHandleEventCounts_WithEvents(t *testing.T) {
	srv, db := newTestServer(t)
	now := time.Now().UTC().Truncate(time.Second)

	seedEvent(t, db, "cnt-1", "front_door", "person", 0.95, now)
	seedEvent(t, db, "cnt-2", "front_door", "car", 0.80, now.Add(-time.Minute))
	seedEvent(t, db, "cnt-3", "back_yard", "person", 0.70, now.Add(-2*time.Minute))

	req := httptest.NewRequest(http.MethodGet, "/api/events/counts", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var body struct {
		Total    int            `json:"total"`
		ByLabel  map[string]int `json:"by_label"`
		ByCamera map[string]int `json:"by_camera"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Total != 3 {
		t.Errorf("total = %d, want 3", body.Total)
	}
	if body.ByLabel["person"] != 2 {
		t.Errorf("by_label[person] = %d, want 2", body.ByLabel["person"])
	}
	if body.ByLabel["car"] != 1 {
		t.Errorf("by_label[car] = %d, want 1", body.ByLabel["car"])
	}
	if body.ByCamera["front_door"] != 2 {
		t.Errorf("by_camera[front_door] = %d, want 2", body.ByCamera["front_door"])
	}
	if body.ByCamera["back_yard"] != 1 {
		t.Errorf("by_camera[back_yard] = %d, want 1", body.ByCamera["back_yard"])
	}
}

func TestHandleEventCounts_NoEvents(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/events/counts", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var body struct {
		Total    int            `json:"total"`
		ByLabel  map[string]int `json:"by_label"`
		ByCamera map[string]int `json:"by_camera"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Total != 0 {
		t.Errorf("total = %d, want 0", body.Total)
	}
}

// --- Events with offset tests ---

func TestHandleListEvents_WithOffset(t *testing.T) {
	srv, db := newTestServer(t)
	now := time.Now().UTC().Truncate(time.Second)

	seedEvent(t, db, "off-1", "cam1", "person", 0.9, now)
	seedEvent(t, db, "off-2", "cam1", "car", 0.8, now.Add(-time.Minute))
	seedEvent(t, db, "off-3", "cam1", "dog", 0.7, now.Add(-2*time.Minute))

	t.Run("offset skips events", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/events?offset=1", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}

		var body struct {
			Events []camera.Event `json:"events"`
		}
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(body.Events) != 2 {
			t.Fatalf("got %d events, want 2", len(body.Events))
		}
		// Events are ordered by timestamp DESC, so skipping the first (newest) gives us off-2 and off-3
		if body.Events[0].ID != "off-2" {
			t.Errorf("first event = %q, want %q", body.Events[0].ID, "off-2")
		}
		if body.Events[1].ID != "off-3" {
			t.Errorf("second event = %q, want %q", body.Events[1].ID, "off-3")
		}
	})

	t.Run("offset and limit combined", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/events?offset=0&limit=1", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}

		var body struct {
			Events []camera.Event `json:"events"`
		}
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(body.Events) != 1 {
			t.Fatalf("got %d events, want 1", len(body.Events))
		}
		if body.Events[0].ID != "off-1" {
			t.Errorf("event = %q, want %q (most recent)", body.Events[0].ID, "off-1")
		}
	})
}

// --- Event Detail Partial with adjacent events ---

func TestHandleEventDetailPartial_AdjacentEvents(t *testing.T) {
	srv, db := newTestServer(t)
	now := time.Now().UTC().Truncate(time.Second)

	seedEvent(t, db, "adj-1", "cam1", "person", 0.9, now.Add(-2*time.Minute))
	seedEvent(t, db, "adj-2", "cam1", "car", 0.8, now.Add(-time.Minute))
	seedEvent(t, db, "adj-3", "cam1", "dog", 0.7, now)

	// Fetch the middle event's detail partial
	req := httptest.NewRequest(http.MethodGet, "/partials/event/adj-2", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	body := w.Body.String()

	// The template renders prev/next as links with data-prev-id and data-next-id attributes
	if !contains(body, "data-prev-id=\"adj-1\"") {
		t.Errorf("response missing previous event link to adj-1, body: %s", body)
	}
	if !contains(body, "data-next-id=\"adj-3\"") {
		t.Errorf("response missing next event link to adj-3, body: %s", body)
	}
}

func TestHandleEventDetailPartial_FirstEvent_NoPrev(t *testing.T) {
	srv, db := newTestServer(t)
	now := time.Now().UTC().Truncate(time.Second)

	seedEvent(t, db, "first-1", "cam1", "person", 0.9, now.Add(-time.Minute))
	seedEvent(t, db, "first-2", "cam1", "car", 0.8, now)

	// Fetch the first (oldest) event
	req := httptest.NewRequest(http.MethodGet, "/partials/event/first-1", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	body := w.Body.String()

	// No previous link (disabled span), but next link should exist
	if contains(body, "data-prev-id") {
		t.Error("first event should not have a previous link")
	}
	if !contains(body, "data-next-id=\"first-2\"") {
		t.Errorf("response missing next event link to first-2, body: %s", body)
	}
}

func TestHandleEventDetailPartial_LastEvent_NoNext(t *testing.T) {
	srv, db := newTestServer(t)
	now := time.Now().UTC().Truncate(time.Second)

	seedEvent(t, db, "last-1", "cam1", "person", 0.9, now.Add(-time.Minute))
	seedEvent(t, db, "last-2", "cam1", "car", 0.8, now)

	// Fetch the last (newest) event
	req := httptest.NewRequest(http.MethodGet, "/partials/event/last-2", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	body := w.Body.String()

	// Previous link should exist, no next link
	if !contains(body, "data-prev-id=\"last-1\"") {
		t.Errorf("response missing previous event link to last-1, body: %s", body)
	}
	if contains(body, "data-next-id") {
		t.Error("last event should not have a next link")
	}
}

// --- Recording export tests ---

func TestHandleRecordingExport_MissingParams(t *testing.T) {
	srv, _ := newTestServer(t)

	tests := []struct {
		name string
		url  string
	}{
		{"no params", "/api/recordings/export/cam1"},
		{"missing end", "/api/recordings/export/cam1?start=2025-01-01T00:00:00Z"},
		{"missing start", "/api/recordings/export/cam1?end=2025-01-01T01:00:00Z"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			w := httptest.NewRecorder()
			srv.mux.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestHandleRecordingExport_InvalidTimeFormat(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet,
		"/api/recordings/export/cam1?start=not-a-time&end=2025-01-01T01:00:00Z", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleRecordingExport_EndBeforeStart(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet,
		"/api/recordings/export/cam1?start=2025-01-01T02:00:00Z&end=2025-01-01T01:00:00Z", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleRecordingExport_RangeExceeds24h(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet,
		"/api/recordings/export/cam1?start=2025-01-01T00:00:00Z&end=2025-01-03T00:00:00Z", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}

	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["error"] != "export range limited to 24 hours" {
		t.Fatalf("unexpected error: %s", body["error"])
	}
}

func TestHandleRecordingExport_NoSegments(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet,
		"/api/recordings/export/cam1?start=2025-01-01T00:00:00Z&end=2025-01-01T01:00:00Z", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}

	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["error"] == "" {
		t.Fatal("expected error message in response body")
	}
}

// --- PTZ API tests ---

// newTestServerWithPTZ creates a test server with a camera that has PTZ support.
// It starts a mock ONVIF server that accepts SOAP requests, registers a camera
// in the manager, and wires a PTZClient pointing at the mock.
func newTestServerWithPTZ(t *testing.T) (*Server, *storage.DB) {
	t.Helper()

	db, err := storage.New(":memory:")
	if err != nil {
		t.Fatalf("create in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mgr := camera.NewManager(nil, nil, config.MotionConfig{}, nil, nil, nil, nil, "", 85, "", nil, nil, "", nil)
	mgr.AddCamera(config.CameraConfig{Name: "ptz_cam", URL: "rtsp://localhost/stream"})
	mgr.AddCamera(config.CameraConfig{Name: "no_ptz_cam", URL: "rtsp://localhost/stream2"})

	rec := recording.New(config.RecordingConfig{
		Path: t.TempDir(),
	}, config.EventConfig{RetainDays: 90}, db, nil, "")

	// Mock ONVIF server that accepts any SOAP request
	ptzServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`<?xml version="1.0"?><s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body/></s:Envelope>`))
	}))
	t.Cleanup(ptzServer.Close)

	ptzClients := map[string]*camera.PTZClient{
		"ptz_cam": camera.NewTestPTZClient(ptzServer.URL, "TestProfile"),
	}

	apiCfg := config.APIConfig{Host: "127.0.0.1", Port: 0}
	srv := New(apiCfg, nil, db)
	srv.SetSubsystems(mgr, rec, nil, nil, nil, "", "", nil, ptzClients)
	return srv, db
}

func TestPTZMoveUp(t *testing.T) {
	srv, _ := newTestServerWithPTZ(t)

	body := strings.NewReader(`{"action":"move","direction":"up"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/cameras/ptz_cam/ptz", body)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status = %q, want %q", resp["status"], "ok")
	}
}

func TestPTZStop(t *testing.T) {
	srv, _ := newTestServerWithPTZ(t)

	body := strings.NewReader(`{"action":"stop"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/cameras/ptz_cam/ptz", body)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status = %q, want %q", resp["status"], "ok")
	}
}

func TestPTZZoomIn(t *testing.T) {
	srv, _ := newTestServerWithPTZ(t)

	body := strings.NewReader(`{"action":"zoom","direction":"in"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/cameras/ptz_cam/ptz", body)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status = %q, want %q", resp["status"], "ok")
	}
}

func TestPTZCameraNotFound(t *testing.T) {
	srv, _ := newTestServerWithPTZ(t)

	body := strings.NewReader(`{"action":"move","direction":"up"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/cameras/nonexistent/ptz", body)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusNotFound, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["error"] != "camera not found" {
		t.Errorf("error = %q, want %q", resp["error"], "camera not found")
	}
}

func TestPTZNotSupported(t *testing.T) {
	srv, _ := newTestServerWithPTZ(t)

	body := strings.NewReader(`{"action":"move","direction":"up"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/cameras/no_ptz_cam/ptz", body)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["error"] != "camera does not support PTZ" {
		t.Errorf("error = %q, want %q", resp["error"], "camera does not support PTZ")
	}
}

func TestPTZInvalidAction(t *testing.T) {
	srv, _ := newTestServerWithPTZ(t)

	body := strings.NewReader(`{"action":"invalid"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/cameras/ptz_cam/ptz", body)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["error"] != "invalid action" {
		t.Errorf("error = %q, want %q", resp["error"], "invalid action")
	}
}

func TestListCamerasIncludesPTZ(t *testing.T) {
	srv, _ := newTestServerWithPTZ(t)

	req := httptest.NewRequest(http.MethodGet, "/api/cameras", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var cameras []struct {
		Name string `json:"name"`
		PTZ  bool   `json:"ptz"`
	}
	if err := json.NewDecoder(w.Body).Decode(&cameras); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(cameras) != 2 {
		t.Fatalf("got %d cameras, want 2", len(cameras))
	}

	ptzByName := make(map[string]bool)
	for _, c := range cameras {
		ptzByName[c.Name] = c.PTZ
	}

	if !ptzByName["ptz_cam"] {
		t.Error("ptz_cam should have ptz=true")
	}
	if ptzByName["no_ptz_cam"] {
		t.Error("no_ptz_cam should have ptz=false")
	}
}

// contains checks if substr is present in s.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestHandleCameraTimeline_ActivityAndEndTime(t *testing.T) {
	srv, db := newTestServer(t)
	srv.cameras.AddCamera(config.CameraConfig{Name: "cam1", URL: "rtsp://localhost/stream"})

	date := time.Date(2026, 3, 25, 0, 0, 0, 0, time.UTC)

	// Seed a segment
	seedSegment(t, db, "cam1", "/tmp/tl-seg.mp4", date.Add(10*time.Hour), date.Add(11*time.Hour), 1024)

	// Seed motion activity
	if err := db.SaveMotionActivity("cam1", date.Add(10*time.Hour+23*time.Minute), 0.73); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMotionActivity("cam1", date.Add(10*time.Hour+24*time.Minute), 0.12); err != nil {
		t.Fatal(err)
	}

	// Seed an event with end_time
	ev := camera.Event{
		ID:         "evt-tl-1",
		CameraName: "cam1",
		Label:      "person",
		Score:      0.95,
		Timestamp:  date.Add(10*time.Hour + 23*time.Minute),
		EndTime:    date.Add(10*time.Hour + 24*time.Minute),
	}
	if err := db.SaveEvent(ev); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/cameras/cam1/timeline?date=2026-03-25", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var body struct {
		Segments []struct{} `json:"segments"`
		Events   []struct {
			ID      string `json:"id"`
			EndTime string `json:"end_time"`
		} `json:"events"`
		Activity []struct {
			Time  string  `json:"t"`
			Score float64 `json:"s"`
		} `json:"activity"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(body.Activity) != 2 {
		t.Fatalf("got %d activity buckets, want 2", len(body.Activity))
	}
	if body.Activity[0].Score != 0.73 {
		t.Errorf("activity[0].s = %f, want 0.73", body.Activity[0].Score)
	}

	if len(body.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(body.Events))
	}
	if body.Events[0].EndTime == "" {
		t.Error("event end_time should be present")
	}
}

func TestHandlePlaybackM3U8_NotFound(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/cameras/unknown/playback.m3u8?start=2026-01-01T00:00:00Z", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestHandlePlaybackM3U8_MissingStart(t *testing.T) {
	s, _ := newTestServer(t)
	s.cameras.AddCamera(config.CameraConfig{Name: "test"})
	req := httptest.NewRequest("GET", "/api/cameras/test/playback.m3u8", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleSegment_NotFound(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/cameras/test/segments/99999", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}
