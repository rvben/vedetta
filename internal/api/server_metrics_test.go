package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rvben/vedetta/internal/metrics"
)

// requestLogMiddleware records RED metrics (request count + latency) for every
// request that reaches the application, bucketed by status class.
func TestRequestLogMiddlewareRecordsHTTPMetrics(t *testing.T) {
	metrics.ResetForTest()
	t.Cleanup(metrics.ResetForTest)

	h := requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/events", nil))

	var b strings.Builder
	metrics.WriteProm(&b)
	out := b.String()

	if !strings.Contains(out, `vedetta_http_requests_total{status="4xx"} 1`) {
		t.Errorf("expected one 4xx request counted:\n%s", out)
	}
	if !strings.Contains(out, `vedetta_http_request_duration_seconds_count{status="4xx"} 1`) {
		t.Errorf("expected one 4xx latency observation:\n%s", out)
	}
}

// Endpoints excluded from tracing (per shouldTraceRequest) must also be
// excluded from RED metrics: /metrics scrapes, health polls, and long-lived
// SSE/WS/MJPEG streams would otherwise dominate the histogram with
// self-referential or unbounded observations.
//
// Every live transport is listed individually rather than by a representative
// sample. A stream missing from this list contributes one observation the
// length of a viewing session, which does not look like corruption on a
// dashboard - it looks like a latency regression.
func TestRequestLogMiddlewareSkipsExcludedPaths(t *testing.T) {
	metrics.ResetForTest()
	t.Cleanup(metrics.ResetForTest)

	h := requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	excluded := []string{
		"/metrics",
		"/api/health",
		"/api/health/live",
		"/api/health/ready",
		"/api/events/stream",
		"/api/cameras/front/detections",
		"/api/cameras/front/mjpeg",
		"/api/cameras/front/mse/ws",
	}
	for _, path := range excluded {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}

	var b strings.Builder
	metrics.WriteProm(&b)
	if strings.Contains(b.String(), "vedetta_http_requests_total{") {
		t.Errorf("excluded paths must not be counted:\n%s", b.String())
	}
}

// The counterpart to the exclusion list: an ordinary request must still be
// counted. Without this, deleting the metrics call entirely would satisfy the
// exclusion test and leave the RED dashboard silently empty.
func TestRequestLogMiddlewareCountsOrdinaryPaths(t *testing.T) {
	metrics.ResetForTest()
	t.Cleanup(metrics.ResetForTest)

	h := requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// Signalling POSTs sit next to the stream endpoints in the URL space but are
	// ordinary requests, so they must stay in the histogram.
	for _, path := range []string{"/api/cameras", "/api/cameras/front/webrtc/offer"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}

	var b strings.Builder
	metrics.WriteProm(&b)
	if !strings.Contains(b.String(), `vedetta_http_requests_total{status="2xx"} 2`) {
		t.Errorf("ordinary requests must be counted:\n%s", b.String())
	}
}

// Status codes map to their HTTP class label.
func TestStatusClass(t *testing.T) {
	cases := map[int]string{
		200: "2xx", 204: "2xx", 301: "3xx", 404: "4xx", 500: "5xx", 101: "1xx",
	}
	for code, want := range cases {
		if got := statusClass(code); got != want {
			t.Errorf("statusClass(%d) = %q, want %q", code, got, want)
		}
	}
}

// The runtime collector reaches the package /metrics exposition, so standard
// go_* dashboards bind against vedetta without extra wiring.
func TestWritePromIncludesRuntimeMetrics(t *testing.T) {
	// No ResetForTest: the runtime collector is stateless and always emits.
	var b strings.Builder
	metrics.WriteProm(&b)
	out := b.String()
	for _, want := range []string{"go_goroutines", "go_memstats_sys_bytes", "go_gc_duration_seconds_count"} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics exposition missing %q:\n%s", want, out)
		}
	}
}
