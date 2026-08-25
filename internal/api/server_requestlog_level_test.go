package api

import (
	"bytes"
	"io"
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
		name     string
		path     string
		status   int
		elapsed  time.Duration
		expected bool
		want     slog.Level
	}{
		{"health probe is routine", "/api/health/live", http.StatusOK, fast, false, slog.LevelDebug},
		{"metrics scrape is routine", "/metrics", http.StatusOK, fast, false, slog.LevelDebug},
		{"health poll is routine", "/api/health", http.StatusOK, fast, false, slog.LevelDebug},
		{"ordinary request", "/api/cameras", http.StatusOK, fast, false, slog.LevelInfo},
		{"not modified is ordinary", "/camera.html", http.StatusNotModified, fast, false, slog.LevelInfo},

		// Failure outranks routineness: a probe is only boring while it works.
		{"failing probe is not demoted", "/api/health/live", http.StatusInternalServerError, fast, false, slog.LevelError},
		{"client error", "/api/cameras/nope", http.StatusNotFound, fast, false, slog.LevelWarn},
		{"unauthorized", "/api/cameras", http.StatusUnauthorized, fast, false, slog.LevelWarn},
		{"server fault", "/api/cameras", http.StatusInternalServerError, fast, false, slog.LevelError},

		// Server faults outrank slowness, so a slow 500 reads as a fault.
		{"slow request", "/api/cameras", http.StatusOK, slowRequestThreshold, false, slog.LevelWarn},
		{"slow server fault", "/api/cameras", http.StatusInternalServerError, slowRequestThreshold, false, slog.LevelError},
		{"just under the slow threshold", "/api/cameras", http.StatusOK, slowRequestThreshold - time.Millisecond, false, slog.LevelInfo},

		// A stream is meant to stay open; duration says nothing about health.
		{"long-lived sse", "/api/events/stream", http.StatusOK, time.Hour, false, slog.LevelDebug},
		{"long-lived detections sse", "/api/cameras/front_door/detections", http.StatusOK, time.Hour, false, slog.LevelDebug},
		{"long-lived mse websocket", "/api/cameras/front_door/mse/ws", http.StatusOK, time.Hour, false, slog.LevelDebug},
		{"long-lived mjpeg", "/api/cameras/front_door/mjpeg", http.StatusOK, time.Hour, false, slog.LevelDebug},
		// ...but a stream that failed to open is still a fault.
		{"failed stream", "/api/events/stream", http.StatusInternalServerError, time.Hour, false, slog.LevelError},

		// WebRTC and talkback media never cross the HTTP server; only the short
		// signalling POST does, so it is graded as the ordinary request it is.
		// An earlier suffix rule claimed to cover "/webrtc", which matches no
		// registered route and therefore covered nothing.
		{"webrtc signalling is ordinary", "/api/cameras/front_door/webrtc/offer", http.StatusOK, fast, false, slog.LevelInfo},
		{"talkback signalling is ordinary", "/api/cameras/front_door/talkback/offer", http.StatusOK, fast, false, slog.LevelInfo},

		// A probe is boring only while it is fast; a slow one is the server
		// telling you it is struggling.
		{"slow health probe is not demoted", "/api/health/live", http.StatusOK, slowRequestThreshold, false, slog.LevelWarn},

		// A deliberate temporary answer is graded as the request it is. The
		// unmarked row is the control: the same status without the mark stays an
		// error, so the demotion follows the mark and not the status code.
		{"marked startup 503 is ordinary", "/api/cameras", http.StatusServiceUnavailable, fast, true, slog.LevelInfo},
		{"unmarked 503 is a fault", "/api/cameras", http.StatusServiceUnavailable, fast, false, slog.LevelError},
		// The mark suppresses only the failure grading; every other rule still
		// applies, so a startup answer that took a second still reports as slow.
		{"marked but slow", "/api/cameras", http.StatusServiceUnavailable, slowRequestThreshold, true, slog.LevelWarn},
		{"marked on a partial", "/partials/events-gallery", http.StatusServiceUnavailable, fast, true, slog.LevelInfo},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if got := requestLogLevel(req, tt.status, tt.elapsed, tt.expected); got != tt.want {
				t.Errorf("requestLogLevel(%s, %d, %v, expected=%v) = %v, want %v",
					tt.path, tt.status, tt.elapsed, tt.expected, got, tt.want)
			}
		})
	}
}

// The grading rule above is only worth anything if the readiness gate actually
// reaches it, which depends on requestLogMiddleware installing its writer
// directly outside the gate. This drives the real chain so that a wrapper
// inserted between them fails here instead of silently restoring one error line
// per API request for every restart's startup window.
func TestReadinessGate503IsNotAnError(t *testing.T) {
	buf := captureLogs(t)

	s := &Server{} // zero value: subsystems not initialized yet
	h := requestLogMiddleware(s.readyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("readiness gate passed the request through while not ready")
	})))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/cameras", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 from the readiness gate", rec.Code)
	}
	got := findRequestLog(t, buf)
	if got["level"] != "INFO" {
		t.Errorf("level = %v, want INFO: the startup gate answering is not a fault", got["level"])
	}
	if got["status"] != float64(http.StatusServiceUnavailable) {
		t.Errorf("status = %v, want the real 503 to still be recorded", got["status"])
	}
}

// Once subsystems are up, nothing marks a 503, so a genuine one from a handler
// reports as the fault it is. Without this the test above is satisfied by a
// middleware that demoted every 503.
func TestServiceUnavailableAfterStartupIsAnError(t *testing.T) {
	buf := captureLogs(t)

	s := &Server{}
	s.ready.Store(true)
	h := requestLogMiddleware(s.readyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/cameras", nil))

	if got := findRequestLog(t, buf); got["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR for a 503 the readiness gate did not produce", got["level"])
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

// Probe traffic was 95% of a real production access log, so the cost of the
// line that is never written is the cost that dominates. The middleware asks
// the logger whether the graded level is enabled before assembling anything;
// this benchmark is what keeps that guard honest.
//
// Run as: go test ./internal/api/ -bench RequestLogMiddleware -benchmem
func BenchmarkRequestLogMiddlewareSuppressedProbe(b *testing.B) {
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo})))
	b.Cleanup(func() { slog.SetDefault(orig) })

	h := requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/health/live", nil)
	req.Header.Set("User-Agent", "Prometheus/2.51.0")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}

// The counterpart at the other threshold: with debug output requested, the same
// probe writes a full line. Comparing the two shows what the guard is avoiding.
func BenchmarkRequestLogMiddlewareEmittedProbe(b *testing.B) {
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})))
	b.Cleanup(func() { slog.SetDefault(orig) })

	h := requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/health/live", nil)
	req.Header.Set("User-Agent", "Prometheus/2.51.0")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), req)
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
