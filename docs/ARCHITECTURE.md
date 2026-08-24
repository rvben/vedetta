# Architecture

Vedetta is a local-first NVR implemented as one Go service with embedded web
assets and SQLite persistence. Optional native libraries accelerate selected
media or inference operations, but the default deployment does not require a
separate broker, restreamer, database, or Python service.

This document describes the current system. Proposed boundaries are recorded
separately in [architecture decision records](adr/README.md).

## Design principles

1. **A useful default is one process.** Extra workers may extend the appliance,
   but may not become mandatory for a basic camera-to-recording installation.
2. **Recording integrity outranks intelligence.** Detection can degrade or be
   disabled without silently breaking continuous recording.
3. **Keep video local by default.** Network egress must be intentional and
   secrets or private imagery must not leak into metrics, logs, or reports.
4. **Remux before transcoding.** Preserve source media where clients support it;
   spend compute only when a compatibility or storage outcome requires it.
5. **Bound every fan-out.** Slow live viewers, notification providers, or model
   work must not apply unbounded backpressure to camera ingest.
6. **Make failure visible.** Health, recording gaps, reconnects, dropped frames,
   storage pressure, and subsystem degradation need operator-facing signals.
7. **Version durable contracts.** Configuration, database migrations, exported
   APIs, and future worker protocols must evolve explicitly.

## Runtime overview

```text
                         +-----------------------+
                         | HTTP API + embedded UI|
                         | auth, review, config  |
                         +-----------+-----------+
                                     |
RTSP / ONVIF             +-----------v-----------+       MQTT / WebPush
+------------+           | camera + stream state |       +-------------+
| camera(s)  +---------->| bounded media fan-out +------>| integrations|
+------------+           +----+----------+-------+       +-------------+
                              |          |
                    +---------v--+    +--v----------------+
                    | recording  |    | decode + motion   |
                    | fMP4/index |    | detection/tracking|
                    +-----+------+    +---------+----------+
                          |                     |
                    +-----v---------------------v----------+
                    | SQLite + recording/snapshot storage  |
                    +--------------------------------------+
```

Camera connections are shared across bounded consumers. Encoded media can flow
to continuous recording and live outputs without object inference. Detection
consumes decoded frames from the lower-resolution role, uses motion to limit
YOLO work, tracks objects across frames, and produces event/recognition work.

## Media pipeline

### Ingest

`internal/rtsp` owns RTSP source negotiation, reconnect behavior, probing, and
track metadata. `internal/camera` coordinates configured camera lifecycles and
ONVIF/PTZ state. A camera may expose different sources for detection and
recording; on-demand sources use a short steady retry policy while sleeping.

The currently supported video input is H.264. AAC and G.711 audio have
protocol-specific paths described in [Compatibility](COMPATIBILITY.md).

### Recording

`internal/recording` and `internal/media` write fragmented MP4 segments and
maintain a durable segment index. Event artifacts select pre/post media from the
recording history rather than requiring an independent always-on encoder.
Retention combines age policies, a maximum allocation, minimum free-space
protection, emergency cleanup, and optional scheduled recompression.

The invariant is that a completed segment is either durably indexed or can be
reconciled from disk after restart. Recording health is independent of whether
detection produces events.

### Detection and recognition

`internal/media` decodes selected H.264 frames. `internal/detect` performs motion
analysis, YOLOv8 inference, tracking, face work, and model management.
`internal/reid` manages object embeddings and known-object matching.
`internal/event` turns detections into durable events, artifacts, Activities,
notifications, and integration messages. An Activity stays open until its
camera has been quiet for 90 seconds. New or late evidence reopens and extends
it; a periodic sweep finalizes due Activities and queues at most one
Activity-level notification. Review exposes that grouping rule directly.
Operators can exclude an unrelated event without deleting it and restore it
later; both decisions remain attributable correction history, separate from
immutable raw evidence.

The pure-Go and C ONNX Runtime bindings currently execute inference on CPU.
VideoToolbox, VA-API, and NVDEC are decode backends, not detector providers.

## Live and downstream streaming

`internal/stream` exposes multiple views of the same camera state:

- WebRTC for low-latency browser playback;
- MSE over WebSocket and HLS using fragmented MP4;
- MJPEG and snapshots as broad fallbacks; and
- an optional RTSP server with main and `_sub` paths.

Consumers have bounded queues and protocol-specific drop/recovery behavior so a
slow browser or downstream client cannot stall camera ingest. Native AAC is
remuxed where possible; G.711-to-AAC conversion is limited to the HLS
compatibility path and requires `libfdk-aac`.

## State and persistence

SQLite runs in WAL mode through `internal/storage`. It holds events, Activities
and their evidence/correction history, segments, zones and presence, people and
faces, known-object references and sightings, sessions and API tokens, motion
activity, notification preferences, and storage audit data. Media files and
snapshots remain on the configured filesystem.

Schema changes are forward migrations in code. Configuration is currently a
strict YAML document loaded at process start; the future control-plane design
must preserve YAML as a portable representation while adding versions, atomic
writes, validation, and reconciliation. See
[ADR 0004](adr/0004-versioned-configuration-control-plane.md).

## API, UI, and security boundary

`internal/api` serves the OpenAPI-described REST API, server-sent events,
streaming handshakes, and embedded web application. The setup flow, live grid,
recording calendar/timeline, event review, people/faces, system state, and
settings all use the same authenticated service boundary.

Browser sessions use CSRF protection. API tokens carry scopes. Proxy-provided
identity is accepted only from configured trusted proxies. TLS and allowed
origins are configurable. Camera credentials, media, identities, event labels,
and camera names are sensitive even on a private network.

## Observability and resilience

The service exposes liveness, readiness, and authenticated Prometheus metrics.
Optional OpenTelemetry export covers request/event traces and structured logs.
Watchdogs, reconnect counters, recording-gap signals, bounded queues, disk
pressure controls, and graceful shutdown make failures detectable and limit
their blast radius.

External integrations are asynchronous. A slow MQTT broker, push relay, or OTLP
collector must degrade its own feature rather than stop recording.

## Package responsibilities

| Area | Primary packages |
| --- | --- |
| Process wiring and lifecycle | `cmd/vedetta`, `internal/lifecycle`, `internal/watchdog` |
| Configuration and updates | `internal/config`, `internal/update`, `internal/artifact` |
| Camera and media ingest | `internal/camera`, `internal/rtsp`, `internal/media` |
| Detection and identity | `internal/detect`, `internal/reid`, `internal/event` |
| Recording and snapshots | `internal/recording`, `internal/snapshot` |
| Live outputs | `internal/stream` |
| API and web product | `internal/api`, `internal/auth`, `internal/netguard` |
| Persistence | `internal/storage` |
| Integrations and telemetry | `internal/mqtt`, `internal/notify`, `internal/metrics`, `internal/tracing`, `internal/logging`, `internal/otelexport` |

Dependencies should point toward small interfaces at subsystem boundaries.
Process wiring belongs in `cmd/vedetta`; storage types, HTTP handlers, and camera
transport details should not become the shared domain model.

## Current domain evolution

The architecture keeps the reliable core and is adding three boundaries in
order:

1. an aggregate **Activity** model groups nearby, camera-local detection and
   doorbell evidence into durable incidents, exposes open/finalized lifecycle
   state over REST and SSE, and sends one notification per finalized incident
   while preserving the raw event API; operator corrections, rules, and
   automation follow on this boundary;
2. a versioned **configuration control plane** that can safely validate, diff,
   apply, roll back, and reconcile changes; and
3. an optional **inference provider/worker contract** so hardware-specific
   acceleration can fail independently while the default CPU appliance remains
   complete.

These are product boundaries, not a service decomposition target. Their ADRs
define the constraints that must be proven before implementation.
