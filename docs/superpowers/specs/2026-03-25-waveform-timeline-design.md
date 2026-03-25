# Waveform Timeline Design

Replace the flat recording-bar timeline with an audio-waveform style visualization where vertical bar heights represent motion activity levels, and event periods are highlighted with a distinct color.

## Current State

The camera page timeline (`internal/api/static/`) renders:
- **Recording segments**: flat cyan rectangles spanning recorded time ranges
- **Event markers**: 8px colored dots at event timestamps (clickable, with tooltips)
- **Playhead**: vertical cyan line with dot cap

The timeline API (`GET /api/cameras/{name}/timeline?date=YYYY-MM-DD`) returns:
```json
{
  "segments": [{"start_time": "...", "end_time": "..."}],
  "events": [{"id": "...", "label": "...", "score": 0.95, "timestamp": "..."}]
}
```

Events have `end_time` in the database and `camera.Event` struct but it is not exposed in the timeline API response.

## Design

### Backend: Motion Activity Storage

**New table** `motion_activity`:
```sql
CREATE TABLE IF NOT EXISTS motion_activity (
    camera TEXT NOT NULL,
    bucket DATETIME NOT NULL,
    score  REAL NOT NULL,
    PRIMARY KEY (camera, bucket)
)
```

- `bucket`: minute-aligned UTC timestamp (e.g., `2026-03-25T14:23:00Z`)
- `score`: 0.0–1.0, the maximum total foreground coverage observed across all frames in that minute
- Minutes with no motion get no row (implicitly 0)

**Score definition:** For each frame, compute `totalForegroundPixels / totalPixels` (the fraction of the frame covered by motion). Store the max across all frames in the minute. This correlates with visual activity better than region fill ratio (`MotionRegion.Score`), which measures compactness rather than coverage — a small, compact motion region scores high on fill ratio but represents little visual activity.

**Why max, not mean:** Max captures peak activity. A minute with one significant motion event and 59 seconds of quiet should still show as active. Mean would wash out brief but significant motion.

**Accumulation in Camera struct** (`internal/camera/camera.go`):
- Add fields: `motionBucketTime time.Time` (minute-truncated), `motionBucketMax float64`
- In `processFrame()`, after motion detection, compute frame coverage and compare against `motionBucketMax`
- When `time.Now().Truncate(time.Minute)` differs from `motionBucketTime`, flush the previous bucket to the DB via a new channel/callback, then reset
- On camera shutdown, flush any pending bucket
- Motion accumulation happens inside the `detectEnabled` block (after `motionDetector.Detect()`). Cameras with detection disabled will have no waveform data — this is acceptable since those cameras also have no events to visualize.
- The `MotionDetector.Detect()` method already computes a `totalFG` count internally. Expose this as a return value or add a method to retrieve frame coverage from the last detection call.

**New DB methods** (`internal/storage/db.go`):
- `SaveMotionActivity(camera string, bucket time.Time, score float64) error` — UPSERT (INSERT OR REPLACE)
- `GetMotionActivity(camera string, date time.Time) ([]MotionBucket, error)` — returns all rows for a UTC day

```go
type MotionBucket struct {
    Bucket time.Time
    Score  float64
}
```

**Retention:** Add `DeleteMotionActivityBefore(cutoff time.Time) error` to `storage.DB`. Call it from `Recorder.runCleanup()` in `internal/recording/retention.go` using the same `RetainDays` cutoff as segment retention, since motion data is meaningless without corresponding recordings. Motion activity rows are tiny and not counted toward `enforceStorageCap()` calculations.

### Backend: Timeline API Extension

Extend `handleCameraTimeline` response to include:

```json
{
  "segments": [...],
  "events": [
    {"id": "...", "label": "...", "score": 0.95, "timestamp": "...", "end_time": "..."}
  ],
  "activity": [
    {"t": "2026-03-25T14:23:00Z", "s": 0.73},
    {"t": "2026-03-25T14:24:00Z", "s": 0.12}
  ]
}
```

- `activity`: sparse array of non-zero minute buckets. Short keys (`t`, `s`) to minimize payload.
- `end_time` added to event objects so the frontend knows the event duration for coloring bars.

### Frontend: Canvas Waveform

**HTML change** (`camera.html`): Add a `<canvas>` element inside `.timeline-track`, before the playhead div.

```html
<div class="timeline-track" id="timeline-track" ...>
  <canvas id="timeline-canvas"></canvas>
  <div class="timeline-playhead" id="timeline-playhead" ...></div>
</div>
```

**CSS changes** (`style.css`):
- Increase `.timeline-track` height from 48px to 56px
- Canvas fills the track absolutely, z-index below playhead and event dots
- Remove `.timeline-segment` styles (no longer used)
- Add `--event-bar: var(--amber)` CSS variable for event-colored bars

**Rendering** (`app.js`):
- New function `renderWaveform(activity, events)` replaces `renderTimelineSegments()`
- Build a 1440-element array (one per minute), zero-filled, populated from sparse `activity` data
- Build a Set of minutes that overlap with any event (using `timestamp` to `end_time` range)
- Draw on canvas:
  - For each minute with score > 0: draw a vertical bar centered on the midline (mirrored above and below)
  - Bar width: `canvasWidth / 1440` (sub-pixel, canvas handles anti-aliasing)
  - Bar height: `score * maxHalfHeight` (where `maxHalfHeight = canvasHeight / 2`), with a minimum of 15% of `maxHalfHeight` to ensure even faint motion is visible rather than invisible
  - Bar color: `--cyan-dim` for normal minutes, `--event-bar` (amber) for minutes overlapping events
- Handle high-DPI displays: set canvas width/height to `trackWidth * devicePixelRatio`, scale context accordingly
- Re-render on window resize (debounced) since canvas dimensions are set programmatically

**Event dots:** Keep as HTML overlays (not drawn on canvas). This preserves existing click handlers, tooltips, and hover effects with zero changes to `renderTimelineEvents()`.

**Existing interactions preserved:**
- Scrubbing, cursor, preview thumbnail — all operate on the `.timeline-track` element, unaffected by canvas
- Playhead animation — DOM element with z-index above canvas
- Event dot clicks/tooltips — remain HTML elements

### Fallback Behavior

When `activity` data is empty (no motion data yet, or historical days before this feature):
- Fall back to flat bars at 50% height for recording segments (similar to current look)
- Uses the existing `segments` data to determine which minutes have coverage

## Files to Modify

| File | Changes |
|------|---------|
| `internal/storage/db.go` | Add `motion_activity` table, `SaveMotionActivity()`, `GetMotionActivity()`, `MotionBucket` type, retention cleanup |
| `internal/camera/camera.go` | Add per-minute motion accumulation, flush on minute boundary |
| `internal/api/server.go` | Extend `handleCameraTimeline` to include `activity` and event `end_time` |
| `internal/api/static/camera.html` | Add `<canvas>` inside timeline track |
| `internal/api/static/app.js` | New `renderWaveform()`, update `fetchTimelineData()` to pass activity data |
| `internal/api/static/style.css` | Track height increase, canvas styling, remove `.timeline-segment`, add `--event-bar` |
| `internal/recording/retention.go` | Call `DeleteMotionActivityBefore()` in `runCleanup()` |
| `internal/detect/motion.go` | Expose frame coverage (totalFG/totalPixels) from `Detect()` |

## Out of Scope

- Per-camera motion sensitivity calibration for waveform display
- Zoom/pan on the timeline
- Backfilling motion data for historical recordings
