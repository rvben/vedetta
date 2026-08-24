# Camera setup

Vedetta works best with cameras that expose an H.264 RTSP stream and an ONVIF
profile. A low-resolution substream for detection and a high-resolution main
stream for recording give the best resource usage.

See [Compatibility](COMPATIBILITY.md) for codec and platform boundaries. Camera
models are only described as verified after a reproducible community report;
vendor family names below are URL examples, not compatibility guarantees.

## Discover and inspect

Run discovery from a host on the same broadcast network as the cameras:

```sh
vedetta discover
vedetta discover -probe-rtsp
```

The first command queries ONVIF WS-Discovery. `-probe-rtsp` also tries likely
RTSP paths and reports stream metadata. Discovery can be blocked by VLAN
boundaries, firewalls, client isolation, or a vendor setting that disables
ONVIF.

After configuring cameras, inspect the effective stream roles without starting
the server:

```sh
vedetta streams -config config.yml
```

## Configure stream roles

```yaml
cameras:
  - name: front_door
    url: rtsp://viewer:change-me@192.0.2.10:554/stream2
    record_url: rtsp://viewer:change-me@192.0.2.10:554/stream1
    rtsp_transport: tcp
    detect:
      enabled: true
      width: 640
      height: 360
      fps: 5
    record:
      width: 1920
      height: 1080
      fps: 15
```

- `url` is the lower-bandwidth detection and preview stream.
- `record_url` is the higher-quality recording stream. When omitted, `url` is
  used for both roles.
- `rtsp_transport` accepts `tcp`, `udp`, or `auto`. Start with TCP; try UDP when
  a camera produces corrupt or stalled video specifically over TCP.
- `on_demand: true` is intended for sleeping battery cameras whose RTSP path
  exists only during a wake or PIR event. Do not use it to hide an unreliable
  mains-powered camera.

Use camera-local, least-privilege viewer credentials where the vendor supports
them. Keep the configuration file out of source control and restrict its file
permissions because RTSP URLs commonly contain credentials.

## Common RTSP path patterns

Replace placeholders with values from the camera. Firmware and channel numbers
can change the correct path.

| Family | Main stream | Substream |
| --- | --- | --- |
| TP-Link Tapo | `rtsp://<user>:<pass>@<camera-host>:554/stream1` | `rtsp://<user>:<pass>@<camera-host>:554/stream2` |
| Reolink | `rtsp://<user>:<pass>@<camera-host>:554/h264Preview_01_main` | `rtsp://<user>:<pass>@<camera-host>:554/h264Preview_01_sub` |
| Hikvision | `rtsp://<user>:<pass>@<camera-host>:554/Streaming/Channels/101` | `rtsp://<user>:<pass>@<camera-host>:554/Streaming/Channels/102` |
| Dahua | `rtsp://<user>:<pass>@<camera-host>:554/cam/realmonitor?channel=1&subtype=0` | `rtsp://<user>:<pass>@<camera-host>:554/cam/realmonitor?channel=1&subtype=1` |

Select H.264 in the camera's own settings. H.265/HEVC is not currently
supported by Vedetta. If possible, use fixed frame rates and keyframe intervals
close to one or two seconds for predictable startup and seeking.

## ONVIF and PTZ

ONVIF discovery can supply profiles and stream URLs. Vedetta also exposes
manual PTZ controls for compatible cameras. Support for discovery does not
guarantee support for every optional ONVIF service, and Vedetta does not yet
perform PTZ autotracking.

Keep ONVIF on a trusted camera network. Some cameras use different credentials
for ONVIF and their mobile app, and some require ONVIF to be explicitly enabled.

## RTSP republishing

Vedetta can republish configured cameras so that downstream tools pull one
normalized source instead of opening more connections to each camera:

```yaml
rtsp_server:
  enabled: true
  port: 8554
```

For a camera named `front_door`, the paths are:

| Path | Source |
| --- | --- |
| `rtsp://<vedetta-host>:8554/front_door` | `record_url`, or `url` when no recording stream is set |
| `rtsp://<vedetta-host>:8554/front_door_sub` | `url`, when it differs from `record_url` |

When `auth.users` is configured, RTSP clients use the same username and
password with Basic authentication. With no configured users, the republish
server is open to its reachable network. `/api/streaming/capabilities` lists
the live URLs Vedetta exposes for each camera.

## Troubleshooting checklist

1. Confirm the URL in VLC or another RTSP client from the Vedetta host.
2. Confirm the stream is H.264, not H.265/HEVC.
3. Check that the configured width, height, and frame rate describe the actual
   stream.
4. Try TCP first, then UDP if the camera has transport-specific problems.
5. Confirm OpenH264 is available on the system/setup page when snapshots and
   detection are unavailable but recording still connects.
6. Check `/api/health/ready`, the system page, and authenticated `/metrics` for
   reconnects, dropped frames, decode latency, and recording gaps.
7. If a battery camera normally sleeps, set `on_demand: true` and test during a
   real wake event.

## Reporting compatibility

Use the **Camera compatibility report** issue form. Remove public IP addresses,
local IP addresses, MAC addresses, usernames, passwords, serial numbers, and
private snapshots before posting. Include model, firmware, stream codecs and
resolutions, transport, ONVIF/PTZ results, and a redacted log excerpt.
