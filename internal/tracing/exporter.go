package tracing

import (
	"context"
	"errors"
	"strings"

	"github.com/rvben/vedetta/internal/otelexport"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

var errNoEndpoint = errors.New("tracing: no OTLP endpoint configured")

// resolvedEndpoint is the shared classification of an OTLP endpoint.
type resolvedEndpoint = otelexport.Endpoint

// resolveEndpoint applies config-then-env precedence and classifies the
// endpoint form. getenv is injected for testability. The config/env values are
// trimmed here (the shared classifier is pure and does not trim) to preserve the
// existing behavior the regression tests rely on.
func resolveEndpoint(cfg Config, getenv func(string) string) (resolvedEndpoint, error) {
	ep := strings.TrimSpace(cfg.Endpoint)
	if ep == "" {
		ep = strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	}
	if ep == "" {
		return resolvedEndpoint{}, errNoEndpoint
	}
	return otelexport.Classify(ep, cfg.Insecure), nil
}

// resolveProtocol applies config-then-env precedence for the OTLP transport.
func resolveProtocol(cfg Config, getenv func(string) string) string {
	p := otelexport.ParseProtocol(cfg.Protocol)
	if p == "" {
		p = otelexport.ParseProtocol(getenv("OTEL_EXPORTER_OTLP_PROTOCOL"))
	}
	return p
}

// buildExporter constructs the OTLP span exporter for the configured transport
// and endpoint.
func buildExporter(ctx context.Context, cfg Config, getenv func(string) string) (sdktrace.SpanExporter, error) {
	re, err := resolveEndpoint(cfg, getenv)
	if err != nil {
		return nil, err
	}
	if resolveProtocol(cfg, getenv) == "grpc" {
		var opts []otlptracegrpc.Option
		if re.AsURL {
			opts = append(opts, otlptracegrpc.WithEndpointURL(re.Value))
		} else {
			opts = append(opts, otlptracegrpc.WithEndpoint(re.Value))
			if re.Insecure {
				opts = append(opts, otlptracegrpc.WithInsecure())
			}
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlptracegrpc.WithHeaders(cfg.Headers))
		}
		return otlptracegrpc.New(ctx, opts...)
	}
	// default: OTLP/HTTP ("http", "http/protobuf", or unset)
	var opts []otlptracehttp.Option
	if re.AsURL {
		opts = append(opts, otlptracehttp.WithEndpointURL(otelexport.WithSignalPath(re.Value, "/v1/traces")))
	} else {
		opts = append(opts, otlptracehttp.WithEndpoint(re.Value))
		if re.Insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlptracehttp.WithHeaders(cfg.Headers))
	}
	return otlptracehttp.New(ctx, opts...)
}
