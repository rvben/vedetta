# PTZ Camera Controls

Add pan/tilt/zoom controls for ONVIF-capable cameras. Controls appear below the live video on the camera detail page. The backend communicates with cameras via ONVIF PTZ SOAP over HTTP.

## Scope

**In scope:**
- ONVIF PTZ capability detection at startup
- ContinuousMove/Stop for pan, tilt, and zoom
- REST API endpoint for PTZ commands
- D-pad + zoom UI below the live video
- Keyboard shortcuts (arrow keys, +/-)
- PTZ capability exposed in camera status API

**Out of scope (future work):**
- Presets (save/recall camera positions)
- Speed slider (fixed speed 0.5 for now)
- Click-to-center (requires AbsoluteMove, spotty support)
- RelativeMove / AbsoluteMove

## Backend

### New file: `internal/camera/ptz.go`

#### PTZClient struct

Manages ONVIF PTZ communication for a single camera.

```go
type PTZClient struct {
    ptzURL       string   // ONVIF PTZ service endpoint
    profileToken string   // media profile token with PTZ config
    username     string
    password     string
    clockOffset  time.Duration // camera clock offset for WS-Security
    httpClient   *http.Client
}
```

#### Capability detection

Called once per camera at startup. The sequence:

1. **GetSystemDateAndTime** (no auth) — `http://<host>/onvif/device_service`
   - Compute clock offset between local time and camera time
   - Used for WS-Security digest computation
   - If this fails (network error, unsupported), assume zero offset and continue
2. **GetCapabilities(All)** (authenticated) — same endpoint
   - Look for `<tt:PTZ><tt:XAddr>` in response
   - If absent, camera does not support PTZ — skip it
3. **GetProfiles** — media service XAddr from capabilities
   - Find first profile containing a `<tt:PTZConfiguration>` element
   - The profile's `token` attribute becomes the ProfileToken for all PTZ commands
   - Example XML path: `Envelope > Body > GetProfilesResponse > Profiles[token="..."] > PTZConfiguration`

If any step fails or PTZ is not advertised, the camera is marked as non-PTZ. No error is raised — PTZ is optional. A `slog.Debug` line records why detection was skipped.

```go
func NewPTZClient(rtspURL string) (*PTZClient, error)
func (c *PTZClient) Available() bool
```

The constructor derives the ONVIF HTTP endpoint from the RTSP URL (same host, port 80). Credentials are extracted from the RTSP URL's userinfo.

#### Authentication

SOAP requests try HTTP Basic Auth first (matching the existing pattern in `onvif_events.go`). If a camera rejects Basic Auth with a SOAP fault, fall back to WS-Security UsernameToken with PasswordDigest:

```
nonce = random 20 bytes
created = camera-clock-adjusted UTC timestamp (ISO 8601)
digest = Base64(SHA1(nonce_raw + created_bytes + password_bytes))
```

The auth mode (basic vs WS-Security) is determined during capability detection and cached for subsequent requests. A fresh nonce is generated for every WS-Security request.

#### ContinuousMove

```go
func (c *PTZClient) ContinuousMove(panSpeed, tiltSpeed, zoomSpeed float64) error
```

- Velocity values range from -1.0 to 1.0
- Always includes both `<PanTilt>` and `<Zoom>` elements with `xmlns="http://www.onvif.org/ver10/schema"` (required by many cameras)
- Includes `<Timeout>PT5S</Timeout>` as a safety net — camera stops automatically if Stop command is lost
- SOAP action: `http://www.onvif.org/ver20/ptz/wsdl/ContinuousMove`

#### Stop

```go
func (c *PTZClient) Stop() error
```

- Stops all pan/tilt/zoom movement
- Sets both `<PanTilt>true</PanTilt>` and `<Zoom>true</Zoom>` for explicitness
- SOAP action: `http://www.onvif.org/ver20/ptz/wsdl/Stop`

#### Direction mapping

Fixed speed of 0.5 for all directions:

| Direction | PanTilt x | PanTilt y | Zoom x |
|-----------|-----------|-----------|--------|
| up        | 0.0       | 0.5       | 0.0    |
| down      | 0.0       | -0.5      | 0.0    |
| left      | -0.5      | 0.0       | 0.0    |
| right     | 0.5       | 0.0       | 0.0    |
| zoom_in   | 0.0       | 0.0       | 0.5    |
| zoom_out  | 0.0       | 0.0       | -0.5   |

### Integration with camera startup

In `cmd/vedetta/main.go` `initSubsystems`:

- After creating the camera manager, iterate over enabled cameras
- Attempt `NewPTZClient` for each camera
- Store PTZ clients in a map `map[string]*PTZClient` (camera name -> client)
- Pass the PTZ client map to the API server as a parameter in `SetSubsystems()` (extending the existing call, consistent with how other subsystems are wired)

The PTZ probe runs concurrently for all cameras (one goroutine per camera) and must not block startup. If a camera is unreachable or doesn't support PTZ, it's silently skipped with a debug log.

## API

### New endpoint

```
POST /api/cameras/{name}/ptz
```

Request body:

```json
{"action": "move", "direction": "up"}
{"action": "move", "direction": "down"}
{"action": "move", "direction": "left"}
{"action": "move", "direction": "right"}
{"action": "zoom", "direction": "in"}
{"action": "zoom", "direction": "out"}
{"action": "stop"}
```

Responses:
- `200 OK` — command sent successfully
- `400 Bad Request` — invalid action/direction, or camera exists but is not PTZ-capable
- `404 Not Found` — camera not found

The endpoint requires authentication (same as all other API endpoints).

### Changes to existing endpoints

`GET /api/cameras` — add `"ptz": true/false` to each camera object in the response. The UI reads this on the camera detail page to decide whether to show PTZ controls.

## Frontend

### UI placement

Below the existing `.live-toolbar`, inside `.live-container`. Only rendered when the camera has `ptz: true`.

```html
<div class="ptz-controls" id="ptz-controls">
  <div class="ptz-dpad">
    <button class="btn btn-sm btn-icon ptz-btn" data-ptz="up" aria-label="Pan up" title="Pan up">
      <svg><!-- chevron-up --></svg>
    </button>
    <button class="btn btn-sm btn-icon ptz-btn" data-ptz="left" aria-label="Pan left" title="Pan left">
      <svg><!-- chevron-left --></svg>
    </button>
    <button class="btn btn-sm btn-icon ptz-btn ptz-stop" data-ptz="stop" aria-label="Stop" title="Stop">
      <svg><!-- square/stop --></svg>
    </button>
    <button class="btn btn-sm btn-icon ptz-btn" data-ptz="right" aria-label="Pan right" title="Pan right">
      <svg><!-- chevron-right --></svg>
    </button>
    <button class="btn btn-sm btn-icon ptz-btn" data-ptz="down" aria-label="Pan down" title="Pan down">
      <svg><!-- chevron-down --></svg>
    </button>
  </div>
  <div class="ptz-zoom">
    <button class="btn btn-sm btn-icon ptz-btn" data-ptz="zoom_in" aria-label="Zoom in" title="Zoom in (+)">
      <svg><!-- plus/zoom-in --></svg>
    </button>
    <button class="btn btn-sm btn-icon ptz-btn" data-ptz="zoom_out" aria-label="Zoom out" title="Zoom out (-)">
      <svg><!-- minus/zoom-out --></svg>
    </button>
  </div>
</div>
```

### CSS layout

```css
.ptz-controls {
  display: flex;
  align-items: center;
  gap: 1rem;
  padding: 0.5rem 0.75rem;
  border-top: 1px solid var(--border-subtle);
}

.ptz-dpad {
  display: grid;
  grid-template-areas:
    ".    up   ."
    "left stop right"
    ".    down .";
  grid-template-columns: repeat(3, 2rem);
  grid-template-rows: repeat(3, 2rem);
  gap: 2px;
}

.ptz-btn[data-ptz="up"]    { grid-area: up; }
.ptz-btn[data-ptz="left"]  { grid-area: left; }
.ptz-btn[data-ptz="stop"]  { grid-area: stop; }
.ptz-btn[data-ptz="right"] { grid-area: right; }
.ptz-btn[data-ptz="down"]  { grid-area: down; }

.ptz-zoom {
  display: flex;
  flex-direction: column;
  gap: 2px;
}
```

On mobile (`max-width: 768px`), button sizes increase to 2.5rem for touch targets.

### JavaScript interaction

**Press-and-hold for continuous movement:**

```javascript
// On pointerdown: send move command
// On pointerup/pointerleave: send stop command
```

Using `pointerdown`/`pointerup` events (works for both mouse and touch). Each button sends a POST to `/api/cameras/{name}/ptz` with the appropriate action.

A 100ms rate limit prevents flooding: if a command was sent within the last 100ms, the new one is dropped. The 5-second safety timeout on ContinuousMove prevents stuck movement if a Stop is lost.

**Keyboard shortcuts (camera page only):**

| Key | Action |
|-----|--------|
| Arrow Up | Pan up (hold) |
| Arrow Down | Pan down (hold) |
| Arrow Left | Pan left (hold) |
| Arrow Right | Pan right (hold) |
| `+` / `=` | Zoom in (hold) |
| `-` | Zoom out (hold) |

Arrow key shortcuts use `keydown`/`keyup` events. `keydown` sends a move command only if `event.repeat` is false (prevents repeat-induced command spam). `keyup` sends stop. Only active when PTZ controls are visible and no input/textarea element is focused.

### Conditional rendering

On camera page load, fetch camera status. If `ptz: true`, remove `hidden` class from `#ptz-controls`. The element starts hidden by default.

## Testing

### Unit tests

- `internal/camera/ptz_test.go`:
  - WS-Security digest computation against known test vectors
  - SOAP XML generation for ContinuousMove, Stop
  - Capability detection XML parsing (with PTZ, without PTZ, missing PTZ element)
  - GetProfiles XML parsing — extract profile token from response with PTZConfiguration
  - Clock offset calculation from GetSystemDateAndTime response
  - Auth fallback: verify Basic Auth tried first, WS-Security on fault

### API tests

- `internal/api/server_test.go`:
  - PTZ endpoint returns 404 for non-PTZ camera
  - PTZ endpoint returns 400 for invalid action
  - PTZ endpoint returns 200 for valid commands (with mock PTZ client)
  - Camera status includes `ptz` field

### Manual testing

- Verify with a real PTZ camera (Tapo C225 or similar ONVIF PTZ camera)
- Confirm press-and-hold moves continuously, release stops
- Confirm keyboard arrows work
- Confirm non-PTZ cameras show no PTZ controls

## File changes

| File | Change |
|------|--------|
| `internal/camera/ptz.go` | **New** — PTZClient, ONVIF SOAP, WS-Security |
| `internal/camera/ptz_test.go` | **New** — unit tests |
| `internal/camera/manager.go` | Add PTZ client storage and `PTZCapable()` |
| `internal/api/server.go` | Add PTZ endpoint, expose PTZ in camera status |
| `internal/api/server_test.go` | Add PTZ API tests |
| `internal/api/server.go` | Extend `SetSubsystems()` signature to include PTZ clients |
| `internal/api/static/camera.html` | Add PTZ controls HTML |
| `internal/api/static/style.css` | Add PTZ control styles |
| `internal/api/static/app.js` | Add PTZ interaction (pointer events, keyboard) |
| `cmd/vedetta/main.go` | Initialize PTZ clients at startup |
| `config.example.yml` | No changes needed — PTZ is auto-detected |

## Compatibility notes

- **ONVIF must be enabled** on the camera (disabled by default on Hikvision, Dahua, Reolink)
- **SOAP 1.2** (`application/soap+xml`) is used, not SOAP 1.1
- **Namespace on velocity elements** (`xmlns="http://www.onvif.org/ver10/schema"`) is always included — some cameras (Reolink) reject requests without it
- **Profile tokens vary** across manufacturers (`MainStream`, `Profile_1`, `000`, etc.) — always discovered via GetProfiles, never hardcoded
- The 5-second ContinuousMove timeout is a conservative safety net; if real-world testing shows it's too short for smooth operation, it can be increased
