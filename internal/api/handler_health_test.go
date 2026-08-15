package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rvben/vedetta/internal/camera"
)

func TestGetHealth_RecompressionStats(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest("GET", "/api/health", nil)
	w := httptest.NewRecorder()
	srv.GetHealth(w, req)

	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode health response: %v", err)
	}

	checks, ok := body["checks"].(map[string]any)
	if !ok {
		t.Fatalf("health response missing checks map")
	}
	storageMap, ok := checks["storage"].(map[string]any)
	if !ok {
		t.Fatalf("health checks missing storage map")
	}
	recompression, ok := storageMap["recompression"].(map[string]any)
	if !ok {
		t.Fatalf("health storage missing recompression map")
	}
	for _, key := range []string{
		"clips_recompressed", "transcode_failures", "worker_crashes", "worker_timeouts",
	} {
		v, ok := recompression[key]
		if !ok {
			t.Errorf("health storage.recompression missing %s", key)
			continue
		}
		n, ok := v.(float64)
		if !ok {
			t.Errorf("%s not a JSON number: %T", key, v)
			continue
		}
		if n != 0 {
			t.Errorf("%s = %v, want 0 for unseeded recorder", key, n)
		}
	}
}

// assertSingleTypeBeforeSamples verifies that the metrics body contains exactly
// one "# TYPE <name> <typ>" line and that it appears before the first sample
// line for the family. A sample line is one that starts with "<name> " or
// "<name>{".
func assertSingleTypeBeforeSamples(t *testing.T, body, name, typ string) {
	t.Helper()
	typeLine := "# TYPE " + name + " " + typ
	lines := strings.Split(body, "\n")

	typeIdx := -1
	for i, l := range lines {
		if l == typeLine {
			if typeIdx != -1 {
				t.Errorf("metric %q: duplicate # TYPE line at lines %d and %d", name, typeIdx, i)
			}
			typeIdx = i
		}
	}
	if typeIdx == -1 {
		t.Errorf("metric %q: missing %q", name, typeLine)
		return
	}

	sampleIdx := -1
	for i, l := range lines {
		if strings.HasPrefix(l, name+" ") || strings.HasPrefix(l, name+"{") {
			sampleIdx = i
			break
		}
	}
	if sampleIdx == -1 {
		// No samples is acceptable for empty families (e.g. stream_clients with
		// zero viewers).  The TYPE line alone is still valid.
		return
	}
	if typeIdx >= sampleIdx {
		t.Errorf("metric %q: # TYPE at line %d must precede first sample at line %d", name, typeIdx, sampleIdx)
	}
}

func TestGetMetricsExposition(t *testing.T) {
	srv, _ := newTestServer(t)
	cam := camera.NewTestCamera("front")
	srv.cameras.RegisterForTest(cam)

	rec := httptest.NewRecorder()
	srv.GetMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()

	assertSingleTypeBeforeSamples(t, body, "vedetta_up", "gauge")
	assertSingleTypeBeforeSamples(t, body, "vedetta_camera_online", "gauge")
	assertSingleTypeBeforeSamples(t, body, "vedetta_camera_reconnects_total", "counter")
	// Renamed gauges must be present with the new names.
	assertSingleTypeBeforeSamples(t, body, "vedetta_events", "gauge")
	assertSingleTypeBeforeSamples(t, body, "vedetta_segments", "gauge")
	// Old _total names must be gone.
	if strings.Contains(body, "vedetta_events_total") || strings.Contains(body, "vedetta_segments_total") {
		t.Errorf("old _total metric names must be gone:\n%s", body)
	}
}

func TestGetMetricsStreamClients(t *testing.T) {
	srv, _ := newTestServer(t)
	cam := camera.NewTestCamera("front")
	srv.cameras.RegisterForTest(cam)

	srv.mjpegViewers.add("front")
	srv.mjpegViewers.add("front")

	// Two distinct HLS clients keep separate RemoteAddr keys; a repeated
	// hit from the first does not double-count it.
	srv.hlsViewers.seen("front", "192.0.2.1:5001")
	srv.hlsViewers.seen("front", "192.0.2.1:5001")
	srv.hlsViewers.seen("front", "198.51.100.5:33000")

	rec := httptest.NewRecorder()
	srv.GetMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()

	if !strings.Contains(body, `vedetta_stream_clients{camera="front",transport="mjpeg"} 2`) {
		t.Fatalf("missing mjpeg stream_clients series:\n%s", body)
	}
	if !strings.Contains(body, `vedetta_stream_clients{camera="front",transport="hls"} 2`) {
		t.Fatalf("missing hls stream_clients series:\n%s", body)
	}
	assertSingleTypeBeforeSamples(t, body, "vedetta_stream_clients", "gauge")
}
