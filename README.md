# Vedetta

Vedetta is a local-first network video recorder for people who want a capable
Frigate-class system without operating a collection of services. It combines
camera ingest, recording, review, live video, object detection, and home
automation in one Go application.

Vedetta is under active development. Read [Compatibility](docs/COMPATIBILITY.md)
before choosing cameras or hardware for a production installation.

## Why Vedetta

- **Recording and review:** incident-sized Activity review with explicit
  collecting/finalized state, one notification per incident, inspectable raw
  evidence, explainable grouping, reversible operator evidence corrections,
  continuous fragmented-MP4 recording, event clips with pre/post capture,
  snapshots, calendar and timeline navigation, ranged export, retention
  policies, disk-pressure protection, and optional tiered recompression.
- **Live video:** WebRTC, Media Source Extensions (MSE), HLS, MJPEG, snapshots,
  and an optional RTSP republish server for downstream consumers.
- **Camera support:** separate low-resolution detection and high-resolution
  recording streams, ONVIF discovery and PTZ, immediate image-rich doorbell
  alerts, manual doorbell entry from the camera grid, a dedicated answer view,
  ONVIF Profile T talkback, and on-demand handling for sleeping battery cameras.
- **Local intelligence:** motion-gated YOLOv8 detection, greedy IoU object
  tracking, zones and presence, face recognition, and object re-identification.
- **Operations:** guided setup, an installable web app, Prometheus metrics,
  optional OpenTelemetry traces and logs, health probes, and recording-gap and
  storage safeguards.
- **Security:** browser sessions with CSRF protection, scoped API tokens,
  trusted-proxy authentication, configurable CORS, and optional TLS.

## Current boundaries

Vedetta is deliberately honest about what it does not support yet:

- H.264 is the supported video codec; HEVC/H.265 ingest is not implemented.
- The standard detector backends execute inference on CPU. `codecs.hwaccel`
  accelerates H.264 **decoding**, not object detection.
- There is no birdseye compositor, PTZ autotracking, license-plate recognition,
  audio-event detection, semantic search, or camera-scoped role model yet.
- Camera behavior varies by firmware. ONVIF or RTSP support on a product page is
  not a substitute for a tested compatibility report.

The [roadmap](docs/ROADMAP.md) explains which gaps matter next and why.

## Quick start

### Docker

```sh
docker run -d \
  --name vedetta \
  --network host \
  -v vedetta-config:/config \
  -v vedetta-data:/data \
  ghcr.io/rvben/vedetta:latest
```

Host networking gives Vedetta direct access to RTSP cameras and ONVIF multicast
discovery. A `docker-compose.yml` is also included. On first run, open
`http://<host>:5050` and complete the setup wizard.

### Native build

```sh
make build
./build/vedetta -config config.yml
```

Go 1.26 or newer is required to build the current tree. Release artifacts are
available on the [GitHub Releases page](https://github.com/rvben/vedetta/releases).

On first use, Vedetta can download pinned detection models and, when enabled,
the OpenH264 runtime. Downloaded artifacts are size-limited and checksum
verified. Offline installations can provide the model and codec library ahead
of time; see [Camera setup](docs/CAMERAS.md) and
[Hardware decoding](docs/HARDWARE_DECODE.md).

## Minimal configuration

Vedetta reads one YAML file. The example below uses the documentation-only
`192.0.2.0/24` address range:

```yaml
cameras:
  - name: front_door
    url: rtsp://viewer:change-me@192.0.2.10:554/stream2
    record_url: rtsp://viewer:change-me@192.0.2.10:554/stream1
    detect:
      enabled: true
      width: 640
      height: 360
      fps: 5
    record:
      width: 1920
      height: 1080
      fps: 15
    zones:
      - name: approach
        points:
          - [0.10, 0.50]
          - [0.90, 0.50]
          - [0.90, 1.00]
          - [0.10, 1.00]
        labels: [person]

recording:
  path: ./recordings
  continuous: true
  retain_days: 7
  event_retain_days: 30
  min_disk_free: 2GB

storage:
  db_path: ./vedetta.db

api:
  host: 0.0.0.0
  port: 5050
  exposure: lan
```

See [`config.example.yml`](config.example.yml) for every setting. Use
`vedetta discover -probe-rtsp` to discover ONVIF cameras and probe likely RTSP
streams, then `vedetta streams` to inspect configured stream roles.

### Detection and decode

```yaml
detect:
  model_path: ""        # empty uses the managed YOLOv8n model
  score_threshold: 0.5
  motion:
    pixel_threshold: 25
    min_area: 200
    background_alpha: 0.05
    min_region_score: 0.02

codecs:
  hwaccel: auto          # auto | software | videotoolbox | vaapi | nvdec
  openh264:
    auto_install: true
```

The default build uses the pure-Go ONNX Runtime binding. `make build-capi`
builds the C API variant. Both currently run inference on CPU. VideoToolbox is
available on macOS; VA-API and NVDEC require the opt-in Linux hardware build.

### MQTT and Home Assistant

```yaml
mqtt:
  enabled: true
  host: 192.0.2.20
  port: 1883
  topic: vedetta
```

Vedetta publishes detections and Home Assistant MQTT discovery records. Keep
the broker on a trusted network and configure its authentication separately.

### Authentication

```yaml
auth:
  users:
    - username: admin
      password_hash: "<bcrypt hash>"
```

Generate a password hash with:

```sh
vedetta auth hash-password 'a-long-unique-password'
```

Automation clients should use scoped API tokens. A `metrics:read` token can
scrape `/metrics` without access to recordings, snapshots, or identities.

## Operations

- `/api/health/live` reports process liveness.
- `/api/health/ready` reports whether the service is ready for traffic.
- `vedetta healthcheck` probes liveness on the port the config declares and
  exits non-zero when the server does not answer. The container images use it
  as their `HEALTHCHECK`, so no HTTP client is needed in the runtime image.
- `vedetta --version` prints the build identity to quote in a bug report.
- `/metrics` exposes authenticated Prometheus metrics.
- Optional OTLP export covers HTTP/event traces and structured logs.
- The OpenAPI contract lives at [`internal/api/openapi.yaml`](internal/api/openapi.yaml).

Treat camera URLs, snapshots, recordings, face data, and telemetry labels as
sensitive. The [security policy](SECURITY.md) describes private reporting and
deployment expectations.

## Development

```sh
make build          # build the default binary
make build-capi     # build with the C ONNX Runtime binding
make test           # JavaScript unit tests and Go tests
make test-browser   # Playwright browser tests
make bench          # detector benchmarks
make lint           # golangci-lint
make vet            # go vet
make vulncheck      # govulncheck, reachable vulnerabilities only
make check          # lint, vet, and both test suites, offline
```

`make check` is the pre-push gate and runs offline. `make vulncheck` is
separate because it downloads the Go vulnerability database and its result
changes when an advisory is published rather than when the code changes. CI
runs it as its own job. Run it before proposing a dependency bump.

Start with [Contributing](CONTRIBUTING.md), then read the
[architecture](docs/ARCHITECTURE.md) and [architecture decisions](docs/adr/README.md).
Camera reports have their own structured issue template.

## Project documents

- [Compatibility](docs/COMPATIBILITY.md)
- [Camera setup](docs/CAMERAS.md)
- [Architecture](docs/ARCHITECTURE.md)
- [Benchmarking](docs/BENCHMARKING.md)
- [Roadmap](docs/ROADMAP.md)
- [Support](SUPPORT.md)
- [Security](SECURITY.md)
- [Code of Conduct](CODE_OF_CONDUCT.md)

## License

Vedetta is licensed under the [Apache License 2.0](LICENSE).
