package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Access logging is graded by what a request means, because logging every one
// at Info made health probes and the metrics scrape 95% of a production log.
// The grading must not simply mute probes: a probe that fails or a request that
// stalls is exactly what an operator needs to see.
func TestRequestLogLevelGrading(t *testing.T) {
	const fast = 5 * time.Millisecond

	tests := []struct {
		name    string
		path    string
		status  int
		elapsed time.Duration
		want    slog.Level
	}{
		{"health probe is routine", "/api/health/live", http.StatusOK, fast, slog.LevelDebug},
		{"metrics scrape is routine", "/metrics", http.StatusOK, fast, slog.LevelDebug},
		{"health poll is routine", "/api/health", http.StatusOK, fast, slog.LevelDebug},
		{"ordinary request", "/api/cameras", http.StatusOK, fast, slog.LevelInfo},
		{"not modified is ordinary", "/camera.html", http.StatusNotModified, fast, slog.LevelInfo},

		// Failure outranks routineness: a probe is only boring while it works.
		{"failing probe is not demoted", "/api/health/live", http.StatusInternalServerError, fast, slog.LevelError},
		{"client error", "/api/cameras/nope", http.StatusNotFound, fast, slog.LevelWarn},
		{"unauthorized", "/api/cameras", http.StatusUnauthorized, fast, slog.LevelWarn},
		{"server fault", "/api/cameras", http.StatusInternalServerError, fast, slog.LevelError},

		// Server faults outrank slowness, so a slow 500 reads as a fault.
		{"slow request", "/api/cameras", http.StatusOK, slowRequestThreshold, slog.LevelWarn},
		{"slow server fault", "/api/cameras", http.StatusInternalServerError, slowRequestThreshold, slog.LevelError},
		{"just under the slow threshold", "/api/cameras", http.StatusOK, slowRequestThreshold - time.Millisecond, slog.LevelInfo},

		// A stream is meant to stay open; duration says nothing about health.
		{"long-lived sse", "/api/events/stream", http.StatusOK, time.Hour, slog.LevelDebug},
		{"long-lived detections sse", "/api/cameras/front_door/detections", http.StatusOK, time.Hour, slog.LevelDebug},
		{"long-lived mse websocket", "/api/cameras/front_door/mse/ws", http.StatusOK, time.Hour, slog.LevelDebug},
		{"long-lived webrtc", "/api/cameras/front_door/webrtc", http.StatusOK, time.Hour, slog.LevelDebug},
		// ...but a stream that failed to open is still a fault.
		{"failed stream", "/api/events/stream", http.StatusInternalServerError, time.Hour, slog.LevelError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if got := requestLogLevel(req, tt.status, tt.elapsed); got != tt.want {
				t.Errorf("requestLogLevel(%s, %d, %v) = %v, want %v",
					tt.path, tt.status, tt.elapsed, got, tt.want)
			}
		})
	}
}

// The volume fix: routine probe traffic must not reach an Info-level log at all.
// The debug half is the positive control - without it, a middleware that logged
// nothing whatsoever would pass the first assertion.
func TestRequestLogMiddlewareSuppressesProbeNoiseAtInfo(t *testing.T) {
	serve := func(buf *safeBuffer) {
		h := requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/health/live", nil))
	}

	atInfo := captureLogs(t)
	serve(atInfo)
	if bytes.Contains(atInfo.snapshot(), []byte("http request")) {
		t.Errorf("health probe reached the Info log, want it suppressed:\n%s", atInfo)
	}

	atDebug := captureLogsAt(t, slog.LevelDebug)
	serve(atDebug)
	if !bytes.Contains(atDebug.snapshot(), []byte("http request")) {
		t.Errorf("health probe missing from the debug log, want it recorded:\n%s", atDebug)
	}
}

// User-Agent and the two cache headers are most of a line's length and say
// nothing about a request that worked, so ordinary traffic logs without them.
func TestRequestLogMiddlewareOmitsClientDetailForOrdinaryTraffic(t *testing.T) {
	buf := captureLogs(t)

	h := requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/cameras", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) Safari")
	req.Header.Set("If-None-Match", `"abc123"`)
	h.ServeHTTP(httptest.NewRecorder(), req)

	got := findRequestLog(t, buf)
	if got["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", got["level"])
	}
	// The line must still identify the request itself.
	if got["uri"] != "/api/cameras" {
		t.Errorf("uri = %v, want /api/cameras", got["uri"])
	}
	for _, field := range []string{"ua", "if_none_match", "cache_control"} {
		if _, present := got[field]; present {
			t.Errorf("field %q present on an ordinary request, want it omitted: %v", field, got)
		}
	}
}

// A request that went wrong is the one case where identifying the caller is
// worth the bytes, so the detail comes back regardless of level.
func TestRequestLogMiddlewareKeepsClientDetailOnFailure(t *testing.T) {
	buf := captureLogs(t)

	h := requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/cameras", nil)
	req.Header.Set("User-Agent", "probe/1.0")
	h.ServeHTTP(httptest.NewRecorder(), req)

	got := findRequestLog(t, buf)
	if got["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", got["level"])
	}
	if got["ua"] != "probe/1.0" {
		t.Errorf("ua = %v, want probe/1.0", got["ua"])
	}
}
