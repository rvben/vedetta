package logging

import (
	"log/slog"
	"strings"

	"github.com/rvben/vedetta/internal/otelexport"
)

// Config mirrors config.LoggingConfig plus the tracing transport fallback. Kept
// here so the logging package does not import config. The Fallback* fields carry
// the tracing exporter's transport so that, when logging specifies no endpoint of
// its own, logs reuse tracing's endpoint, protocol, and insecure flag as one unit
// rather than a mismatched mix.
type Config struct {
	Enabled bool
	// Level is the minimum severity the OTLP arm exports. Nil means Info. Pass
	// the same *slog.LevelVar the local handler uses so one setting governs both
	// arms and a level change cannot leave them disagreeing.
	Level            slog.Leveler
	Endpoint         string
	Protocol         string
	Insecure         bool
	ServiceName      string
	Headers          map[string]string // sent on every OTLP export request (e.g. X-Scope-OrgID for multi-tenant Loki)
	FallbackEndpoint string            // tracing endpoint, used when Endpoint is empty
	FallbackProtocol string            // tracing protocol, used when Endpoint is empty
	FallbackInsecure bool              // tracing insecure flag, used when Endpoint is empty
}

// usingFallback reports whether logs have no endpoint of their own and will
// reuse the tracing endpoint. Endpoint/protocol/insecure fallback are coupled
// through this predicate so the tracing transport is reused atomically.
func usingFallback(cfg Config) bool {
	return strings.TrimSpace(cfg.Endpoint) == "" && strings.TrimSpace(cfg.FallbackEndpoint) != ""
}

// resolveEndpoint decides the explicit OTLP endpoint for logs. It only ever
// promotes a config-supplied endpoint (the logging endpoint, then the tracing
// fallback) to an explicit exporter option. When neither is set it returns
// explicit=false, so Init builds the exporter without an endpoint option and
// the SDK applies its own env precedence (OTEL_EXPORTER_OTLP_LOGS_ENDPOINT over
// OTEL_EXPORTER_OTLP_ENDPOINT). The generic env is never read here, because
// doing so and passing it explicitly would override the SDK's signal-specific
// precedence. Config/fallback values are trimmed so a whitespace endpoint
// defers to env rather than becoming a bogus explicit option. When the tracing
// endpoint is reused, its paired insecure flag is used (not the logging one),
// so a scheme-less plaintext tracing collector is not probed over TLS.
func resolveEndpoint(cfg Config) (ep otelexport.Endpoint, explicit bool) {
	if raw := strings.TrimSpace(cfg.Endpoint); raw != "" {
		return otelexport.Classify(raw, cfg.Insecure), true
	}
	if raw := strings.TrimSpace(cfg.FallbackEndpoint); raw != "" {
		return otelexport.Classify(raw, cfg.FallbackInsecure), true
	}
	return otelexport.Endpoint{}, false
}

// resolveProtocol picks the OTLP transport for logs. The protocol selects the
// exporter constructor, so it must be decided up front. Logging's own protocol
// wins, then logs-specific env. When logs reuse the tracing endpoint, tracing's
// protocol is reused next - ahead of the generic env - so a generic
// OTEL_EXPORTER_OTLP_PROTOCOL cannot pair the wrong wire with the tracing
// endpoint; the transport stays atomic. The tracing protocol is consulted only
// while falling back, so it never leaks onto a logging endpoint of its own.
// Otherwise the generic env applies, then the default http/protobuf.
func resolveProtocol(cfg Config, getenv func(string) string) string {
	if p := otelexport.ParseProtocol(cfg.Protocol); p != "" {
		return p
	}
	if p := otelexport.ParseProtocol(getenv("OTEL_EXPORTER_OTLP_LOGS_PROTOCOL")); p != "" {
		return p
	}
	if usingFallback(cfg) {
		if p := otelexport.ParseProtocol(cfg.FallbackProtocol); p != "" {
			return p
		}
	}
	if p := otelexport.ParseProtocol(getenv("OTEL_EXPORTER_OTLP_PROTOCOL")); p != "" {
		return p
	}
	return "http/protobuf"
}
