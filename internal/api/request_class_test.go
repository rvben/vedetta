package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func (c requestClass) String() string {
	switch c {
	case classOrdinary:
		return "ordinary"
	case classProbe:
		return "probe"
	case classStream:
		return "stream"
	}
	return "unknown"
}

// classifyRequest is the single source of truth behind tracing, RED metrics and
// access-log grading, so a mistake here is a mistake in all three at once.
func TestClassifyRequest(t *testing.T) {
	tests := []struct {
		path string
		want requestClass
	}{
		{"/metrics", classProbe},
		{"/api/health", classProbe},
		{"/api/health/live", classProbe},
		{"/api/health/ready", classProbe},

		{"/api/events/stream", classStream},
		{"/api/cameras/front_door/detections", classStream},
		{"/api/cameras/front_door/mjpeg", classStream},
		{"/api/cameras/front_door/mse/ws", classStream},

		{"/api/cameras", classOrdinary},
		{"/api/cameras/front_door", classOrdinary},
		{"/api/cameras/front_door/snapshot", classOrdinary},
		{"/camera.html", classOrdinary},
		{"/api/streaming/capabilities", classOrdinary},

		// Signalling is a short POST that returns an answer immediately; the
		// media rides a peer connection that never touches the HTTP server.
		// Classing these as streams would drop real request latency on the floor.
		{"/api/cameras/front_door/webrtc/offer", classOrdinary},
		{"/api/cameras/front_door/talkback/offer", classOrdinary},

		// HLS is polling, but each fetch is a short, bounded request whose
		// latency is exactly what playback quality depends on, so it stays in
		// the histogram as ordinary work.
		{"/api/cameras/front_door/live.m3u8", classOrdinary},
		{"/api/cameras/front_door/live/init.mp4", classOrdinary},
		{"/api/cameras/front_door/live/42.m4s", classOrdinary},

		// The camera-scoped stream suffixes must not match outside their
		// prefix, or an unrelated future route ending in "/mjpeg" silently
		// leaves the metrics.
		{"/mjpeg", classOrdinary},
		{"/api/events/detections", classOrdinary},

		// Not a registered route. It must classify as ordinary so that the 401
		// it actually returns is reported, rather than being quietly demoted by
		// a rule that pattern-matched the word "ready".
		{"/api/ready", classOrdinary},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if got := classifyRequest(req); got != tt.want {
				t.Errorf("classifyRequest(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

// shouldTraceRequest gates both spans and RED metrics, so it must admit exactly
// the ordinary class and nothing else.
func TestShouldTraceRequestAdmitsOnlyOrdinary(t *testing.T) {
	cases := map[string]bool{
		"/api/cameras":                         true,
		"/api/cameras/front_door/webrtc/offer": true,
		"/metrics":                             false,
		"/api/health/live":                     false,
		"/api/events/stream":                   false,
		"/api/cameras/front_door/mjpeg":        false,
		"/api/cameras/front_door/mse/ws":       false,
		"/api/cameras/front_door/detections":   false,
	}
	for path, want := range cases {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if got := shouldTraceRequest(req); got != want {
			t.Errorf("shouldTraceRequest(%q) = %v, want %v", path, got, want)
		}
	}
}
