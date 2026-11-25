package api

import (
	"net/http"
	"strings"
)

// requestClass describes the shape of an inbound request, so tracing, RED
// metrics and access logging can each decide what to do from one shared
// description.
//
// The single description is the point. The same knowledge previously lived in
// two path lists that had already drifted: long-lived streams were being timed
// as if they were ordinary requests, one clause matched no registered route at
// all, and the MJPEG stream appeared in neither list. A class says what a
// request *is*; each consumer decides separately what that means for it.
type requestClass int

const (
	// classOrdinary is ordinary API and UI work: it starts, does something and
	// finishes. Worth a span, a latency observation and a log line.
	classOrdinary requestClass = iota

	// classProbe is unattended machine polling: health checks and the metrics
	// scrape. It is the bulk of request volume on a running install and carries
	// no information while it succeeds. Its latency is still real, so the
	// slow-request rule continues to apply to it.
	classProbe

	// classStream stays open for a time the client chooses: server-sent events,
	// WebSocket, multipart MJPEG, and the timed profile captures. Elapsed time
	// on one of these measures how long the client watched or asked to record,
	// not how slow the server was, so timing it as a request is meaningless and
	// pollutes any latency statistic it enters.
	classStream
)

// classifyRequest reports the shape of r from its path.
//
// Paths are matched against the routes actually registered on the mux. A suffix
// that matches no route is worse than useless: it reads as coverage while
// silently doing nothing, which is how the WebRTC signalling endpoint came to
// look handled when it never was.
func classifyRequest(r *http.Request) requestClass {
	p := r.URL.Path

	switch p {
	case "/metrics", "/api/health", "/api/health/live", "/api/health/ready":
		return classProbe
	case "/api/events/stream":
		return classStream
	}

	// A CPU profile or execution trace runs for a length the caller chose, and
	// so does any other profile asked for an interval, so the elapsed time is a
	// request parameter rather than a latency. A profile without an interval is
	// a snapshot that returns at once.
	if name, ok := strings.CutPrefix(p, "/api/debug/pprof/"); ok {
		if name == "profile" || name == "trace" || r.URL.Query().Has("seconds") {
			return classStream
		}
	}

	// Per-camera live transports. WebRTC and talkback are deliberately absent:
	// their endpoints are short signalling POSTs (".../webrtc/offer",
	// ".../talkback/offer"), and the media itself never crosses the HTTP server.
	if strings.HasPrefix(p, "/api/cameras/") {
		switch {
		case strings.HasSuffix(p, "/detections"),
			strings.HasSuffix(p, "/mjpeg"),
			strings.HasSuffix(p, "/mse/ws"):
			return classStream
		}
	}

	return classOrdinary
}
