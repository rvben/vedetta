package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func newTestTracerProvider(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return exp
}

// Every path below is a route registered on the mux, so a clause that stops
// matching one of them is a real regression rather than a renamed constant.
func TestWithTracingFiltersNoisyEndpoints(t *testing.T) {
	exp := newTestTracerProvider(t)
	s := &Server{tracingEnabled: true}
	h := s.withTracing(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, path := range []string{
		"/metrics",
		"/api/health",
		"/api/health/live",
		"/api/health/ready",
		"/api/events/stream",
		"/api/cameras/front/detections",
		"/api/cameras/front/mjpeg",
		"/api/cameras/front/mse/ws",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
		if n := len(exp.GetSpans()); n != 0 {
			t.Fatalf("%s produced %d spans, want 0", path, n)
		}
	}

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/cameras", nil))
	if n := len(exp.GetSpans()); n != 1 {
		t.Fatalf("normal route produced %d spans, want 1", n)
	}
}

// The live transports are filtered by suffix, so the signalling endpoints that
// merely sit under the same prefix must keep their spans. These are ordinary
// short POSTs that negotiate a session and then return; the media never crosses
// the HTTP server, and a failed or slow negotiation is exactly the kind of thing
// a trace is for.
//
// This is the assertion the previous list got wrong: it filtered
// "/api/cameras/{name}/webrtc", which is not a registered route, so it read as
// coverage while never matching a request any client sends.
func TestWithTracingKeepsSignallingEndpoints(t *testing.T) {
	exp := newTestTracerProvider(t)
	s := &Server{tracingEnabled: true}
	h := s.withTracing(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i, path := range []string{
		"/api/cameras/front/webrtc/offer",
		"/api/cameras/front/talkback/offer",
		"/api/cameras/front/talkback/capabilities",
	} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, nil))
		if n := len(exp.GetSpans()); n != i+1 {
			t.Fatalf("%s produced %d spans in total, want %d", path, n, i+1)
		}
	}
}

func TestWithTracingDisabledNoSpans(t *testing.T) {
	exp := newTestTracerProvider(t)
	s := &Server{tracingEnabled: false}
	h := s.withTracing(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/cameras", nil))
	if n := len(exp.GetSpans()); n != 0 {
		t.Fatalf("disabled tracing produced %d spans, want 0", n)
	}
}
