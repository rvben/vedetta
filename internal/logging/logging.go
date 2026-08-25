// Package logging owns vedetta's optional OTLP log export. It is opt-in and
// default-off: when disabled, Init returns the caller's base slog.Handler
// unchanged with zero overhead. When enabled it tees logs to both the base
// handler (stdout/launchd) and an OTLP arm that ships records to a collector.
// Init never returns a fatal error; construction failures degrade to base-only.
package logging

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"time"

	"github.com/rvben/vedetta/internal/otelexport"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	otlploggrpc "go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	otlploghttp "go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

const instrumentationScope = "github.com/rvben/vedetta"

// exporterFactory builds the OTLP log exporter. It is a package var so tests can
// substitute a deterministic failure to exercise the degrade-to-base path
// (construction rarely fails synchronously in the real exporters).
var exporterFactory = buildExporter

// Provider holds the active log handler and shutdown hooks. Obtain one from Init.
type Provider struct {
	handler  slog.Handler
	shutdown []func(context.Context) error
}

// Handler returns the slog.Handler to install as the process default. Always
// non-nil.
func (p *Provider) Handler() slog.Handler { return p.handler }

// Shutdown flushes buffered log records and runs all shutdown hooks. Bound the
// context at the call site so a blocked exporter cannot wedge process exit.
func (p *Provider) Shutdown(ctx context.Context) error {
	var err error
	for _, fn := range p.shutdown {
		if e := fn(ctx); e != nil {
			err = errors.Join(err, e)
		}
	}
	return err
}

// Init configures OTLP log export per cfg. When disabled it returns the base
// handler unchanged. When enabled it builds an OTLP exporter and tees logs to
// base + a level-gated otelslog arm. Any construction failure degrades to
// base-only and is logged once via base; it is never fatal.
func Init(ctx context.Context, cfg Config, version string, base slog.Handler) (*Provider, error) {
	if !cfg.Enabled {
		return &Provider{handler: base}, nil
	}

	exp, err := exporterFactory(ctx, cfg, os.Getenv)
	if err != nil {
		slog.New(base).Warn("log export disabled: OTLP exporter init failed", "err", err)
		return &Provider{handler: base}, nil
	}

	name := cfg.ServiceName
	if name == "" {
		name = "vedetta"
	}
	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(name),
		semconv.ServiceVersion(version),
	)

	otelexport.InstallRateLimitedErrorHandler(30 * time.Second)

	lp := log.NewLoggerProvider(
		log.WithProcessor(log.NewBatchProcessor(exp)),
		log.WithResource(res),
	)
	otelArm := otelslog.NewHandler(instrumentationScope, otelslog.WithLoggerProvider(lp))
	var min slog.Leveler = slog.LevelInfo
	if cfg.Level != nil {
		min = cfg.Level
	}
	gated := newLevelGate(min, otelArm)

	return &Provider{
		handler:  newFanout(base, gated),
		shutdown: []func(context.Context) error{lp.Shutdown},
	}, nil
}

// buildExporter constructs the OTLP log exporter for the resolved transport and
// endpoint. When no explicit endpoint is configured, no endpoint option is set
// so the exporter applies its own standard env resolution.
func buildExporter(ctx context.Context, cfg Config, getenv func(string) string) (log.Exporter, error) {
	ep, explicit := resolveEndpoint(cfg)
	if resolveProtocol(cfg, getenv) == "grpc" {
		var opts []otlploggrpc.Option
		if explicit {
			if ep.AsURL {
				opts = append(opts, otlploggrpc.WithEndpointURL(ep.Value))
			} else {
				opts = append(opts, otlploggrpc.WithEndpoint(ep.Value))
				if ep.Insecure {
					opts = append(opts, otlploggrpc.WithInsecure())
				}
			}
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlploggrpc.WithHeaders(cfg.Headers))
		}
		return otlploggrpc.New(ctx, opts...)
	}
	var opts []otlploghttp.Option
	if explicit {
		if ep.AsURL {
			opts = append(opts, otlploghttp.WithEndpointURL(ensureLogsPath(ep.Value)))
		} else {
			opts = append(opts, otlploghttp.WithEndpoint(ep.Value))
			if ep.Insecure {
				opts = append(opts, otlploghttp.WithInsecure())
			}
		}
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlploghttp.WithHeaders(cfg.Headers))
	}
	return otlploghttp.New(ctx, opts...)
}

// ensureLogsPath guarantees a URL-form HTTP endpoint carries the /v1/logs path.
// otlploghttp.WithEndpointURL records the parsed URL path as an explicit setting,
// so an empty or root path is taken literally (posting to "/") and the exporter
// does NOT fall back to its default /v1/logs the way the trace exporter does. A
// scheme-less host:port endpoint goes through WithEndpoint instead and keeps the
// default path, so it is not affected. An explicit non-root path is respected.
func ensureLogsPath(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Path != "" && u.Path != "/") {
		return rawURL
	}
	u.Path = "/v1/logs"
	return u.String()
}
