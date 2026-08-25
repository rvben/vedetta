package api

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// shouldTraceRequest reports whether an inbound request should produce a span
// and a RED metrics observation.
//
// Only ordinary requests qualify. Probes would flood a trace backend and make
// idle polling look like load, and a long-lived stream would contribute a
// single observation measuring how long someone watched a camera, which is not
// a latency and skews every percentile it lands in.
func shouldTraceRequest(r *http.Request) bool {
	return classifyRequest(r) == classOrdinary
}

// withTracing wraps h with otelhttp request spans when tracing is enabled.
// When disabled it returns h unchanged so there is zero added overhead.
func (s *Server) withTracing(h http.Handler) http.Handler {
	if !s.tracingEnabled {
		return h
	}
	return otelhttp.NewHandler(h, "vedetta-api", otelhttp.WithFilter(shouldTraceRequest))
}
