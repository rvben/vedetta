# Benchmarking

Vedetta performance claims should describe an entire workload, not just model
inference. Camera count, stream shape, motion rate, storage, live viewers, and
host power policy can change the result more than a detector microbenchmark.

## Baseline workload record

Record these fields with every published result:

| Area | Required data |
| --- | --- |
| Vedetta | version or commit, build tags, configuration diff |
| Host | OS/kernel, architecture, CPU, memory, power mode |
| Acceleration | decode backend, detector backend/provider, driver/runtime versions |
| Cameras | count, codec, main/sub dimensions, FPS, bitrate, audio, transport |
| Activity | motion duty cycle, labels, zones, face/re-ID enabled |
| Recording | segment size, filesystem/device, retention and recompression state |
| Viewing | concurrent WebRTC, MSE, HLS, MJPEG, and RTSP clients |
| Duration | warm-up and measured intervals |

Use synthetic or explicitly authorized footage. Never publish private camera
URLs, credentials, faces, plates, home layouts, or identifiable snapshots.

## Standard scenarios

1. **Idle recording:** all cameras record, no live viewers, minimal motion.
2. **Detection load:** repeatable motion on every detection stream.
3. **Review load:** recording continues while one timeline export and two live
   viewers run.
4. **Storage pressure:** cleanup operates near the configured free-space floor.
5. **Camera failure:** one source disconnects and reconnects while others record.
6. **Provider failure:** detector initialization or execution fails while
   recording and live view continue.

Each scenario should run long enough to include reconnect, segment rotation, and
queue behavior rather than reporting a startup burst.

## Measurements

Capture at least:

- end-to-end detection latency and detector invocation rate;
- decoded and dropped frames per camera;
- recording gaps and segment-finalization latency;
- reconnect count and time to healthy media after reconnect;
- live-view client count, queue drops, and startup time;
- process CPU, resident memory, filesystem throughput, and disk usage; and
- power draw when the host exposes a reliable measurement.

Use authenticated `/metrics` for service measurements and a host-native tool for
resource data. Keep raw results beside the exact configuration used to generate
them.

## Existing commands

```sh
make bench
make test
make test-race
```

`make bench` measures detector implementation details; it is useful for
regressions but is not an appliance capacity result. Full workload automation is
a foundation roadmap deliverable. Until it exists, label manual results as such
and avoid extrapolating from one camera to many.
