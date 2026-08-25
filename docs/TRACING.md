# Distributed Tracing (Design Recommendation)

Status: proposal, not yet implemented.

This document recommends how to add OpenTelemetry distributed tracing to Vedetta
and exports spans to the homelab Grafana Tempo backend. It is a design
recommendation; no tracing code exists in the tree yet.

## Verdict

Yes, tracing is worth adding, but in two scopes with very different cost/value:

1. **HTTP API tracing** - low effort, low risk, immediately useful. Wrap the
   server mux with `otelhttp`, gate it behind config. Recommended for a first
   pass.
2. **Detection-pipeline tracing** - higher value (it shows where per-event
   latency goes: YOLO inference vs. tracking vs. DB/MQTT/clip), but the pipeline
   is goroutine-and-channel based with no `context.Context` flowing through it,
   so it needs deliberate context threading. Recommended as a second phase.

Per-frame spans (motion check, H264 decode, every YOLO call) are explicitly
**not** recommended as always-on. At 5 fps per camera across multiple cameras
that is a high-cardinality firehose with real overhead. Expose them only behind
an opt-in debug sample ratio.

Tracing must be **opt-in (disabled by default)**. Vedetta's first design
principle is zero-config single-binary operation; an unset/false tracing config
must behave exactly as today with zero exporter and zero overhead.

## Backend

The reference deployment runs Grafana Tempo on a separate monitoring host, with
OTLP receivers published on that host:

| Receiver | Port | Use |
|----------|------|-----|
| OTLP gRPC | 4317 | container-internal services on the monitoring network |
| OTLP HTTP | 4318 | everything else, including off-host services |
| Tempo HTTP API | 3200 | Grafana datasource / TraceQL queries |

Vedetta typically runs on its own recorder host rather than on the monitoring
network, so it should use **OTLP/HTTP** to the collector's `:4318` receiver.
Addresses below use the documentation ranges from RFC 5737; substitute your own.
A cross-subnet route to the monitoring host is the normal case and is already
exercised by the MQTT publisher, which reaches its broker the same way. Where
both a LAN address and an overlay-network (Tailscale, WireGuard) address exist,
either works; the overlay address is the more robust choice when the recorder
and collector are not on the same LAN.

No tenant header or auth is required on a trusted LAN. Tempo's metrics-generator
derives RED metrics from the spans, so emitting traces also yields
request/error/duration metrics for free without touching the existing `/metrics`
endpoint.

## Dependencies and build impact

Pure-Go modules, no cgo:

```
go.opentelemetry.io/otel
go.opentelemetry.io/otel/sdk
go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp
go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp   # HTTP middleware
```

These cross-compile cleanly to `darwin/arm64` and do not affect the existing
`make build-deploy` codesign step or the optional `cgo_onnxruntime` build tag.
They are independent of the ONNX backend selection.

## Configuration (gating)

Add a `Tracing` section to `internal/config/config.go` alongside `MQTTConfig`,
defaulted to disabled in `Defaults()`:

```yaml
tracing:
  enabled: false
  endpoint: http://198.51.100.11:4318 # OTLP/HTTP base; /v1/traces is appended
  service_name: vedetta
  sample_ratio: 1.0                   # head sampling for event traces
  trace_frames: false                 # opt-in per-frame spans (debug only)
```

`config.Load()` (config.go:423) validates that `endpoint` is set when
`enabled: true`. There is no env-var substitution in Vedetta today, so the YAML
field is the single knob; keep it that way for consistency.

## Initialization

Mirror the conditional MQTT init in `cmd/vedetta/main.go` (the `mqttClient` block
around main.go:344). After `config.Load()` (main.go:107):

- When `cfg.Tracing.Enabled` is false: do nothing. No provider, no exporter.
- When true: build an `otlptracehttp` exporter pointed at
  `cfg.Tracing.Endpoint`, wrap it in a batch span processor and a
  `sdktrace.TracerProvider` carrying a `service.name` resource, and call
  `otel.SetTracerProvider`. Register the W3C `TraceContext` propagator so inbound
  `traceparent` headers from Caddy are honored.
- Return a shutdown closure that flushes the batch processor, deferred from
  `main`, so buffered spans flush on exit.

A tracing misconfiguration must never crash the NVR. If the exporter fails to
build, log via `slog` and continue with tracing disabled, exactly as the Rust
services do (build returns an error, the service degrades to logging-only rather
than panicking).

## Phase 1: HTTP API spans

Single insertion point. The middleware chain is assembled in
`internal/api/server.go:329`. Wrap the mux with
`otelhttp.NewHandler(handler, "vedetta-api")` as the outermost or
near-outermost layer, or add an explicit span in `requestLogMiddleware`
(server.go:549) which already owns the status-capturing
`statusLoggingResponseWriter`.

`otelhttp` is preferred: it extracts `traceparent` automatically, names spans by
route pattern, and records `http.status_code`, method, and route without manual
plumbing. Skip span creation for the high-frequency, low-value endpoints
(`/metrics`, `/api/health/*`, the SSE detection stream `/api/cameras/{name}/detections`)
via a filter so health polling and the live overlay stream do not flood Tempo.

Value: end-to-end latency for the htmx partials, camera management CRUD, ONVIF
discovery probes (which make outbound HTTP to cameras), and WebRTC negotiation.

## Phase 2: detection-pipeline spans (per event, not per frame)

The meaningful unit to trace is a **detection event** (a new confirmed track),
not a frame. Events are low-volume and already carry a unique identity:
`camera.Event.ID` has the form `{camera}-t{trackID}-{unixMs}` (camera.go:701).

Recommended span tree, rooted when an event is emitted:

```
event {camera, label, track_id}                       (root, created at emitEvent)
├── yolo.infer        detect/detector.go:DetectRGB24:115   (serialized by d.mu; the hot path)
├── track.confirm     detect/tracker.go:Update:128
├── db.save           main.go runEventLoop ~789
├── mqtt.publish      mqtt/client.go (events / label / snapshot topics)
├── notify.push       notify WebPush dispatch
├── face.recognize    detect/face.go:DetectAndEmbed         (when in a face zone)
├── object.reid       main.go:matchEventToKnownObjects:1127 (detached goroutine)
└── clip.extract      recording/clip.go:ExtractClip:17      (detached, post-event +~15s)
```

The challenge: frames flow through goroutines and channels
(`processFrame` -> tracking -> `events` chan -> `runEventLoop`) with no
`context.Context`. Two ways to wire the trace, in order of preference:

1. **Carry context through the event path (preferred).** Add a
   `context.Context` (or just the `trace.SpanContext`) to the `camera.Event`
   struct, started at `emitEvent()` (camera.go:834). The central
   `runEventLoop()` (main.go:686) then derives child spans for DB/MQTT/push, and
   the detached clip and re-ID goroutines continue the trace via the carried
   context. This gives true parent-child linkage end to end. Cost: a struct field
   plus threading context into the few detached goroutines.

2. **Synthetic trace id from `Event.ID` (simpler, weaker).** Derive a
   deterministic trace id by hashing `Event.ID` and emit independent spans per
   stage stamped with it. No struct changes, but you lose automatic parent-child
   nesting and clock-skew handling. Acceptable only if option 1 is deferred.

`yolo.infer` is the span worth watching: inference is serialized across all
cameras behind `d.mu` (detector.go), so its duration and queue wait are the
primary detection-latency signal.

### Per-frame spans: debug-only

`processFrame` (camera.go:523), motion detection (motion.go:71), and H264 decode
(detect_consumer.go:133) run on every frame. Tracing them always-on is the wrong
default. Gate them behind `tracing.trace_frames` plus a low head-sampling ratio
(for example 1 in 1000 frames) so an operator can occasionally inspect decode and
motion timing without paying the cost continuously.

## What not to change

- Keep the hand-rolled `/metrics` text endpoint (handler_health.go:185,
  notify/metrics.go). Tracing is additive; do not migrate metrics to
  `client_golang` as part of this work.
- Keep `slog` as the logging backend. Optionally inject `trace_id`/`span_id`
  into log records for trace-to-log correlation in Grafana, which is a small
  `slog.Handler` wrapper and can come later.

## Suggested rollout

1. Add the `Tracing` config block (disabled default) and the init/shutdown
   wiring in `main.go`. No behavior change while disabled.
2. Phase 1: `otelhttp` on the API mux with the health/metrics/SSE filter. Verify
   spans land in Tempo (`{resource.service.name="vedetta"}` in TraceQL) from a
   real Mac Mini deploy.
3. Phase 2: event-rooted pipeline spans via context on `camera.Event`.
4. Optional: `trace_frames` debug sampling and `slog` trace-id correlation.

## Verification (once implemented)

From the infra LXC, the same checks used for the other traced services:

```bash
# service shows up as a tag value
curl -s 'http://localhost:3200/api/search/tag/service.name/values'
# query its traces
curl -s 'http://localhost:3200/api/search?q=%7Bresource.service.name%3D%22vedetta%22%7D&limit=5'
```
