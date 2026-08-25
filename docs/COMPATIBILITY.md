# Compatibility

This document distinguishes implemented protocol support from camera models
verified in the field. A parser accepting a stream is not the same as a camera
working reliably across reconnects, recording, live view, audio, and firmware
updates.

## Support levels

- **Supported:** implemented, covered by automated tests, and intended for use.
- **Build-specific:** supported only when the named build or host dependency is
  present.
- **Not supported:** rejected or intentionally omitted from the current product.
- **Experimental:** available for evaluation without a stability commitment.

## Media and camera capabilities

| Capability | Level | Notes |
| --- | --- | --- |
| RTSP H.264 video ingest | Supported | Detection, recording, and live paths use H.264. |
| RTSP H.265/HEVC video ingest | Not supported | Discovery can report H.265, but the media pipeline cannot consume it. |
| AAC audio | Supported | Recording, MSE/HLS playback, and RTSP republishing are supported. WebRTC does not pass AAC through. |
| G.711 PCMA/PCMU audio | Supported | WebRTC and RTSP republishing can pass it through. HLS converts it to AAC when `libfdk-aac` is available; otherwise HLS remains video-only. |
| Opus camera audio | Not supported | A probe can identify it, but production live/record paths do not consume it. |
| Separate detect/record streams | Supported | Recommended for efficient detection and high-quality recording. |
| TCP, UDP, and automatic RTSP transport | Supported | Configurable per camera. |
| ONVIF WS-Discovery | Supported | Multicast reachability is required. |
| ONVIF PTZ controls | Supported | Manual controls only; no autotracking. |
| ONVIF Profile T audio backchannel | Experimental | The doorbell answer view supports push-to-talk when the camera advertises a mono 8 kHz G.711 PCMA or PCMU backchannel. Firmware behavior still requires model-specific verification. |
| Sleeping battery cameras | Supported | Use the on-demand camera mode. Exact wake behavior remains vendor-specific. |
| Doorbell press events | Supported | Per-camera webhook and clip behavior, immediate image-rich web push, a dedicated live answer view, and manual entry from configured doorbell camera cards are available. |

## Live and playback outputs

| Output | Level | Notes |
| --- | --- | --- |
| WebRTC | Supported | Lowest-latency browser path; PCMA/PCMU audio passthrough. |
| MSE over WebSocket | Supported | Fragmented MP4; supports native AAC audio. |
| HLS | Supported | Fragmented MP4 HLS with optional G.711-to-AAC conversion. |
| MJPEG | Supported | Broad fallback at higher bandwidth. |
| JPEG snapshot | Supported | Latest camera and event snapshots. |
| RTSP republish | Supported | Main and optional `_sub` paths. |
| Multi-camera birdseye video | Not supported | The dashboard can show cameras, but there is no composited output stream. |

## Decode and inference

| Backend | Platform | Level | Scope |
| --- | --- | --- | --- |
| OpenH264 software | Linux/macOS | Supported | H.264 decode for detection and snapshots. |
| VideoToolbox | macOS | Supported | H.264 hardware decode. |
| VA-API | Linux Intel/AMD | Build-specific | H.264 hardware decode; requires the `hwaccel` build and host libraries/devices. |
| NVDEC | Linux NVIDIA | Build-specific | H.264 hardware decode; requires the `hwaccel` build and NVIDIA runtime access. |
| Pure-Go ONNX Runtime binding | Linux/macOS | Supported | CPU object inference. |
| C ONNX Runtime binding | Linux/macOS | Build-specific | CPU object inference via `make build-capi`. |
| CUDA, TensorRT, OpenVINO, Core ML inference | Any | Not supported | Hardware decode settings do not enable detector acceleration. |
| Remote detector workers | Any | Not supported | The proposed boundary is documented in [ADR 0002](adr/0002-optional-inference-workers.md). |

`codecs.hwaccel: auto` intentionally prefers software decode for the typical
low-resolution, low-frame-rate detection stream. Force a hardware backend only
after measuring the actual workload. See [Hardware decoding](HARDWARE_DECODE.md).

## Distribution targets

| Target | Level | Notes |
| --- | --- | --- |
| Linux amd64 binary | Supported | Built by release CI. |
| Linux arm64 binary | Supported | Built by release CI. |
| Linux amd64/arm64 container | Supported | Default CPU image. |
| Linux amd64 hardware container | Build-specific | VA-API/NVDEC image. |
| macOS native source build | Supported | VideoToolbox is available; no macOS binary is currently attached by release CI. |
| Windows | Not supported | No supported build or deployment path. |

## Camera model verification

The project does not yet claim a verified-model matrix. Common Tapo, Reolink,
Hikvision, and Dahua path patterns are documented in [Camera setup](CAMERAS.md),
but firmware, regional variants, NVR channels, authentication modes, and
vendor-specific wake behavior can change results.

A model becomes **verified** only when a compatibility report includes:

1. exact model and firmware;
2. H.264 main/substream codecs, dimensions, frame rates, and audio codec;
3. stable recording and at least two successful reconnects;
4. live results for WebRTC, MSE/HLS, and snapshot fallback;
5. ONVIF discovery and PTZ results where applicable; and
6. doorbell talkback codec and two-way audio results where applicable; and
7. logs and configuration with all personal data and credentials redacted.

Use the repository's **Camera compatibility report** issue form to contribute a
result. Reports should state partial failures instead of reducing a camera to a
single yes/no badge.
