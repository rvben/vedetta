# Vedetta Architecture

## Overview

Vedetta is a native, cross-platform NVR (Network Video Recorder) with real-time AI
object detection. Written in Go for single-binary distribution.

## Design Principles

1. **Zero-config sensible defaults** — works out of the box with minimal YAML
2. **Single binary** — no Docker, no Python, no containers
3. **Deterministic deployment** — no runtime-downloaded models or codec libraries
4. **Efficient by design** — motion gates detection, re-mux instead of re-encode
5. **Camera-friendly** — separate detect/record streams, ONVIF discovery

## Pipeline Architecture

```
┌─────────────┐     ┌──────────────┐     ┌──────────────┐
│  RTSP Stream │────▶│  Frame       │────▶│  Motion      │
│  (ffmpeg)    │     │  Decoder     │     │  Detector    │
│  Low-res     │     │  RGB24→RGBA  │     │  Contour     │
└─────────────┘     └──────────────┘     └──────┬───────┘
                                                │ motion region
                                                ▼
┌─────────────┐     ┌──────────────┐     ┌──────────────┐
│  RTSP Stream │     │  Object      │◀────│  Region      │
│  (ffmpeg)    │     │  Detector    │     │  Cropper     │
│  High-res    │     │  ONNX/YOLO   │     └──────────────┘
└──────┬──────┘     └──────┬───────┘
       │                   │ detections
       ▼                   ▼
┌──────────────┐    ┌──────────────┐     ┌──────────────┐
│  Segment     │    │  Object      │────▶│  Event       │
│  Recorder    │    │  Tracker     │     │  Processor   │
│  10min .mp4  │    │  IoU-based   │     │  Lifecycle   │
└──────┬──────┘    └──────────────┘     └──────┬───────┘
       │                                       │
       ▼                                       ▼
┌──────────────┐    ┌──────────────┐    ┌──────────────┐
│  Clip        │    │  Snapshot    │    │  Notify      │
│  Extractor   │    │  + BBox      │    │  MQTT/Hook   │
│  Pre+Post    │    │  Overlay     │    │              │
└──────────────┘    └──────────────┘    └──────────────┘
       │                   │                   │
       ▼                   ▼                   ▼
┌─────────────────────────────────────────────────────┐
│                    SQLite (WAL)                      │
│  events │ segments │ cameras │ recordings            │
└─────────────────────────────────────────────────────┘
       │
       ▼
┌─────────────────────────────────────────────────────┐
│                    HTTP API + WebUI                   │
│  REST │ WebRTC live │ MJPEG │ Event browser │ Config │
└─────────────────────────────────────────────────────┘
```

## Key Differences from Frigate

| Aspect | Frigate | Vedetta |
|--------|---------|-----------|
| Distribution | Docker-only | Single binary |
| Language | Python glue + C++ ML | Go + ONNX Runtime |
| macOS support | None | Native with CoreML |
| Camera discovery | Manual config | ONVIF auto-discovery |
| Live streaming | go2rtc sidecar | Embedded WebRTC |
| Config | Static YAML, restart needed | Static YAML + strict validation |
| Hardware accel | Manual per-platform | CPU / installed local decode libraries |
| First-run | Complex YAML required | Discovery + sample config |

## Directory Structure

```
cmd/vedetta/          Entry point
internal/
├── api/                HTTP API + WebSocket
├── camera/             RTSP stream management
│   ├── camera.go       Single camera lifecycle
│   ├── manager.go      Multi-camera orchestration
│   └── onvif.go        ONVIF discovery
├── config/             YAML config + validation
├── detect/
│   ├── detector.go     ONNX Runtime session management
│   ├── motion.go       Contour-based motion detection
│   ├── tracker.go      IoU object tracker
│   ├── yolo.go         YOLOv8 pre/post processing
│   └── labels.go       COCO-80 class labels
├── event/              Event lifecycle, cooldown, recognition + fan-out
├── lifecycle/          Graceful process shutdown coordination
├── mqtt/               MQTT publishing
├── recording/
│   ├── segment.go      Continuous segment recorder
│   ├── clip.go         Event clip extraction
│   └── retention.go    Storage cleanup
├── snapshot/           JPEG snapshots with bbox overlay
├── stream/             WebRTC / MJPEG live streaming
└── storage/            SQLite persistence
```

## Deployment Notes

- Vedetta requires the ONNX model to be bundled or configured via `detect.model_path`.
- Vedetta requires OpenH264 to be installed locally or exposed via `OPENH264_LIB`.
- HLS/LLHLS, hot reload, and an interactive setup wizard are not part of the current shipped architecture.
