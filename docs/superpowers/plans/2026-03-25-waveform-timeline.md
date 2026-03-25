# Waveform Timeline Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the flat recording-bar timeline with an audio-waveform visualization driven by per-minute motion activity data, with event periods highlighted in a distinct color.

**Architecture:** Add a `motion_activity` table to SQLite for per-minute motion scores. Expose `totalFG` from the motion detector. Camera accumulates max frame coverage per minute and sends it via a channel to main.go, which writes to the DB. The timeline API serves this data alongside segments and events. The frontend renders it as a mirrored vertical-bar waveform on a canvas element.

**Tech Stack:** Go (backend), SQLite (storage), vanilla JS + Canvas API (frontend)

**Spec:** `docs/superpowers/specs/2026-03-25-waveform-timeline-design.md`

---

### Task 1: Expose Frame Coverage from Motion Detector

**Files:**
- Modify: `internal/detect/motion.go:69-145`
- Test: `internal/detect/motion_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/detect/motion_test.go`:

```go
func TestMotionDetector_FrameCoverage(t *testing.T) {
	md := NewMotionDetector(25, 50, 0.5) // high bgAlpha for fast adaptation in tests
	w, h := 100, 100

	// First frame: all black (initializes background)
	frame1 := make([]byte, w*h*3)
	md.Detect(frame1, w, h)

	// Second frame: change 20% of pixels to white
	frame2 := make([]byte, w*h*3)
	for i := 0; i < w*h*3/5; i++ {
		frame2[i] = 255
	}
	md.Detect(frame2, w, h)

	coverage := md.FrameCoverage()
	if coverage < 0.1 || coverage > 0.3 {
		t.Errorf("expected frame coverage ~0.2, got %f", coverage)
	}

	// No motion: coverage should be 0 (high bgAlpha adapts quickly)
	for i := 0; i < 10; i++ {
		md.Detect(frame1, w, h)
	}
	coverage = md.FrameCoverage()
	if coverage > 0.05 {
		t.Errorf("expected near-zero coverage after static frames, got %f", coverage)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/detect/ -run TestMotionDetector_FrameCoverage -v`
Expected: FAIL — `md.FrameCoverage undefined`

- [ ] **Step 3: Implement FrameCoverage**

In `internal/detect/motion.go`, add a `lastCoverage` field to `MotionDetector` struct (after line 54, `parent []int`):

```go
lastCoverage float64
```

In the `Detect()` method, after computing `totalFG` (line 121), store the coverage:

```go
m.lastCoverage = float64(totalFG) / float64(pixels)
```

Also set it to 0 at the early returns (line 101 — first frame, and line 128-130 — no foreground):

At line 101, before `return nil`:
```go
m.lastCoverage = 0
```

At line 128, change the `if totalFG == 0` block to:
```go
if totalFG == 0 {
	m.lastCoverage = 0
	return nil
}
```

Add the getter method:

```go
// FrameCoverage returns the fraction of pixels that were classified as
// foreground in the most recent Detect call (0.0–1.0).
func (m *MotionDetector) FrameCoverage() float64 {
	return m.lastCoverage
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/detect/ -run TestMotionDetector_FrameCoverage -v`
Expected: PASS

- [ ] **Step 5: Run all detect tests**

Run: `go test ./internal/detect/ -v`
Expected: All PASS

- [ ] **Step 6: Commit**

```bash
git add internal/detect/motion.go internal/detect/motion_test.go
git commit -m "feat(detect): expose frame coverage from motion detector"
```

---

### Task 2: Add Motion Activity DB Table and Methods

**Files:**
- Modify: `internal/storage/db.go:58-105` (migrate), `internal/storage/db.go:453-461` (near SaveSegment)
- Test: `internal/storage/db_test.go`

- [ ] **Step 1: Write failing tests**

Add to `internal/storage/db_test.go`:

```go
func TestSaveAndGetMotionActivity(t *testing.T) {
	db := newTestDB(t)

	bucket1 := time.Date(2026, 3, 25, 14, 23, 0, 0, time.UTC)
	bucket2 := time.Date(2026, 3, 25, 14, 24, 0, 0, time.UTC)
	bucket3 := time.Date(2026, 3, 26, 10, 0, 0, 0, time.UTC) // different day

	if err := db.SaveMotionActivity("cam1", bucket1, 0.73); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMotionActivity("cam1", bucket2, 0.12); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMotionActivity("cam1", bucket3, 0.50); err != nil {
		t.Fatal(err)
	}

	// Query for March 25
	buckets, err := db.GetMotionActivity("cam1", bucket1)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 2 {
		t.Fatalf("expected 2 buckets, got %d", len(buckets))
	}
	if buckets[0].Score != 0.73 {
		t.Errorf("expected score 0.73, got %f", buckets[0].Score)
	}
	if buckets[1].Score != 0.12 {
		t.Errorf("expected score 0.12, got %f", buckets[1].Score)
	}
}

func TestSaveMotionActivity_Upsert(t *testing.T) {
	db := newTestDB(t)

	bucket := time.Date(2026, 3, 25, 14, 23, 0, 0, time.UTC)

	if err := db.SaveMotionActivity("cam1", bucket, 0.50); err != nil {
		t.Fatal(err)
	}
	// Overwrite with higher score
	if err := db.SaveMotionActivity("cam1", bucket, 0.90); err != nil {
		t.Fatal(err)
	}

	buckets, err := db.GetMotionActivity("cam1", bucket)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 1 {
		t.Fatalf("expected 1 bucket, got %d", len(buckets))
	}
	if buckets[0].Score != 0.90 {
		t.Errorf("expected upserted score 0.90, got %f", buckets[0].Score)
	}
}

func TestDeleteMotionActivityBefore(t *testing.T) {
	db := newTestDB(t)

	old := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 3, 25, 12, 0, 0, 0, time.UTC)
	cutoff := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	if err := db.SaveMotionActivity("cam1", old, 0.5); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMotionActivity("cam1", recent, 0.8); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteMotionActivityBefore(cutoff); err != nil {
		t.Fatal(err)
	}

	buckets, err := db.GetMotionActivity("cam1", old)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 0 {
		t.Errorf("expected 0 buckets after cleanup, got %d", len(buckets))
	}

	buckets, err = db.GetMotionActivity("cam1", recent)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 1 {
		t.Errorf("expected 1 bucket retained, got %d", len(buckets))
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/storage/ -run "TestSaveAndGetMotionActivity|TestSaveMotionActivity_Upsert|TestDeleteMotionActivityBefore" -v`
Expected: FAIL — methods not defined

- [ ] **Step 3: Add MotionBucket type and CREATE TABLE**

In `internal/storage/db.go`, after `SegmentRecord` (after line 30), add:

```go
// MotionBucket represents a per-minute motion activity measurement.
type MotionBucket struct {
	Bucket time.Time
	Score  float64
}
```

In `migrate()`, add inside the multi-statement `db.Exec` block, just before the closing backtick+paren at line 208 (after `auth_users` table):

```go
		CREATE TABLE IF NOT EXISTS motion_activity (
			camera TEXT NOT NULL,
			bucket DATETIME NOT NULL,
			score  REAL NOT NULL,
			PRIMARY KEY (camera, bucket)
		);
```

- [ ] **Step 4: Implement SaveMotionActivity**

Add after `SaveSegment` (after line 461):

```go
// SaveMotionActivity upserts a per-minute motion activity score.
func (d *DB) SaveMotionActivity(camera string, bucket time.Time, score float64) error {
	_, err := d.db.Exec(
		"INSERT OR REPLACE INTO motion_activity (camera, bucket, score) VALUES (?, ?, ?)",
		camera, utc(bucket), score,
	)
	return err
}
```

- [ ] **Step 5: Implement GetMotionActivity**

Add after `SaveMotionActivity`:

```go
// GetMotionActivity returns all motion activity buckets for a camera on a given date.
func (d *DB) GetMotionActivity(camera string, date time.Time) ([]MotionBucket, error) {
	dayStart := date.UTC().Truncate(24 * time.Hour)
	dayEnd := dayStart.Add(24 * time.Hour)

	rows, err := d.db.Query(
		"SELECT bucket, score FROM motion_activity WHERE camera = ? AND bucket >= ? AND bucket < ? ORDER BY bucket",
		camera, dayStart, dayEnd,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var buckets []MotionBucket
	for rows.Next() {
		var b MotionBucket
		if err := rows.Scan(&b.Bucket, &b.Score); err != nil {
			return nil, err
		}
		buckets = append(buckets, b)
	}
	return buckets, rows.Err()
}
```

- [ ] **Step 6: Implement DeleteMotionActivityBefore**

Add after `GetMotionActivity`:

```go
// DeleteMotionActivityBefore removes motion activity data older than cutoff.
func (d *DB) DeleteMotionActivityBefore(cutoff time.Time) error {
	_, err := d.db.Exec("DELETE FROM motion_activity WHERE bucket < ?", utc(cutoff))
	return err
}
```

- [ ] **Step 7: Run tests to verify they pass**

Run: `go test ./internal/storage/ -run "TestSaveAndGetMotionActivity|TestSaveMotionActivity_Upsert|TestDeleteMotionActivityBefore" -v`
Expected: All PASS

- [ ] **Step 8: Run all storage tests**

Run: `go test ./internal/storage/ -v`
Expected: All PASS

- [ ] **Step 9: Commit**

```bash
git add internal/storage/db.go internal/storage/db_test.go
git commit -m "feat(storage): add motion_activity table and methods"
```

---

### Task 3: Add Motion Activity Channel and Accumulation in Camera

**Files:**
- Modify: `internal/camera/camera.go:55-130` (struct + constructor), `internal/camera/camera.go:368-380` (processFrame)
- Modify: `internal/camera/manager.go:14-50` (Manager struct + NewManager + AddCamera)

- [ ] **Step 1: Define MotionActivity type**

In `internal/camera/camera.go`, after the existing type definitions (near line 22), add:

```go
// MotionActivity represents a per-minute motion score for timeline waveforms.
type MotionActivity struct {
	CameraName string
	Bucket     time.Time
	Score      float64
}
```

- [ ] **Step 2: Add fields to Camera struct**

In `internal/camera/camera.go`, add to the Camera struct (after line 70, `motionMinRegionScore`):

```go
	motionActivity   chan<- MotionActivity
	motionBucketTime time.Time
	motionBucketMax  float64
```

- [ ] **Step 3: Add motionActivity channel to NewCamera and Manager**

In `NewCamera()` (line 101), add `motionActivity chan<- MotionActivity` parameter after `faceCropDir string`. Set it in the struct initialization:

```go
motionActivity:       motionActivity,
```

In `internal/camera/manager.go`, add `motionActivity chan<- MotionActivity` field to `Manager` struct and `NewManager` parameters. Pass it through to `NewCamera` calls.

Check where `AddCamera` and any other camera creation calls exist and update them.

- [ ] **Step 4: Add motion accumulation logic to processFrame**

In `internal/camera/camera.go`, add the accumulation logic right after `motionRegions := c.motionDetector.Detect(buf, w, h)` (line 369), **before** the `qualifiedMotion` check. This captures all frame coverage, not just frames that exceed the region score threshold:

```go
	// Accumulate motion activity for timeline waveform
	if c.motionActivity != nil {
		coverage := c.motionDetector.FrameCoverage()
		now := time.Now().Truncate(time.Minute)
		if !c.motionBucketTime.IsZero() && now != c.motionBucketTime {
			// Minute boundary crossed — flush previous bucket
			select {
			case c.motionActivity <- MotionActivity{
				CameraName: c.config.Name,
				Bucket:     c.motionBucketTime,
				Score:      c.motionBucketMax,
			}:
			default:
			}
			c.motionBucketMax = 0
		}
		c.motionBucketTime = now
		if coverage > c.motionBucketMax {
			c.motionBucketMax = coverage
		}
	}
```

The `select/default` prevents blocking if the channel is full.

- [ ] **Step 5: Add shutdown flush to readFrames**

In `readFrames()` (line 259), the goroutine exits when `ctx.Done()` fires (line 294). Add a deferred flush before the `for` loop (after line 291):

```go
	defer c.flushMotionBucket()
```

Add the flush method:

```go
func (c *Camera) flushMotionBucket() {
	if c.motionActivity == nil || c.motionBucketTime.IsZero() || c.motionBucketMax <= 0 {
		return
	}
	select {
	case c.motionActivity <- MotionActivity{
		CameraName: c.config.Name,
		Bucket:     c.motionBucketTime,
		Score:      c.motionBucketMax,
	}:
	default:
	}
}
```

- [ ] **Step 6: Verify compilation**

Run: `go build ./...`
Expected: Compiles without errors. (Tests may need updates for changed signatures — update test helpers that call `NewCamera` or `NewManager` to pass `nil` for the new channel parameter.)

- [ ] **Step 7: Fix any test compilation errors**

Update `internal/camera/camera_test.go` — find calls to `NewCamera` and add `nil` for the `motionActivity` channel parameter. Same for any manager test files.

- [ ] **Step 8: Run camera tests**

Run: `go test ./internal/camera/ -v`
Expected: All PASS

- [ ] **Step 9: Commit**

```bash
git add internal/camera/camera.go internal/camera/manager.go internal/camera/camera_test.go
git commit -m "feat(camera): add per-minute motion activity accumulation"
```

---

### Task 4: Wire Motion Activity Channel in main.go

**Files:**
- Modify: `cmd/vedetta/main.go:45-48` (channel declarations), `cmd/vedetta/main.go:319-322` (channel init), `cmd/vedetta/main.go:517` (event loop)

- [ ] **Step 1: Add channel and DB write to main.go**

In the struct with channels (near line 45), add:

```go
motionActivity chan camera.MotionActivity
```

Where channels are initialized (near line 319), add:

```go
sub.motionActivity = make(chan camera.MotionActivity, 100)
```

Pass `sub.motionActivity` to `NewManager()`.

In `runEventLoop()` (line 433), add a local alias following the existing pattern (lines 434-437):

```go
motionActivity := sub.motionActivity
```

In the event loop `select` (near line 517), add a new case:

```go
case ma := <-motionActivity:
	if err := db.SaveMotionActivity(ma.CameraName, ma.Bucket, ma.Score); err != nil {
		slog.Error("failed to save motion activity", "camera", ma.CameraName, "error", err)
	}
```

- [ ] **Step 2: Verify compilation and run**

Run: `go build ./cmd/vedetta/`
Expected: Compiles without errors

- [ ] **Step 3: Commit**

```bash
git add cmd/vedetta/main.go
git commit -m "feat: wire motion activity channel from cameras to DB"
```

---

### Task 5: Add Retention Cleanup for Motion Activity

**Files:**
- Modify: `internal/recording/retention.go:42-56`

- [ ] **Step 1: Add cleanup call**

In `runCleanup()` (line 52), after `r.cleanEventMetadata(eventMetadataCutoff)`, add:

```go
	r.cleanMotionActivity(segmentCutoff)
```

Add the method:

```go
func (r *Recorder) cleanMotionActivity(cutoff time.Time) {
	if err := r.db.DeleteMotionActivityBefore(cutoff); err != nil {
		slog.Error("failed to delete expired motion activity", "error", err)
	}
}
```

- [ ] **Step 2: Verify compilation**

Run: `go build ./...`
Expected: Compiles without errors

- [ ] **Step 3: Commit**

```bash
git add internal/recording/retention.go
git commit -m "feat(recording): add motion activity retention cleanup"
```

---

### Task 6: Extend Timeline API with Activity and Event End Time

**Files:**
- Modify: `internal/api/server.go:1220-1254`
- Test: `internal/api/server_test.go`

- [ ] **Step 1: Write failing test**

Add to `internal/api/server_test.go`:

```go
func TestHandleCameraTimeline_ActivityAndEndTime(t *testing.T) {
	srv, db := newTestServer(t)

	date := time.Date(2026, 3, 25, 0, 0, 0, 0, time.UTC)

	// Seed a segment
	seedSegment(t, db, "cam1", "/tmp/tl-seg.mp4", date.Add(10*time.Hour), date.Add(11*time.Hour), 1024)

	// Seed motion activity
	if err := db.SaveMotionActivity("cam1", date.Add(10*time.Hour+23*time.Minute), 0.73); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMotionActivity("cam1", date.Add(10*time.Hour+24*time.Minute), 0.12); err != nil {
		t.Fatal(err)
	}

	// Seed an event with end_time
	ev := camera.Event{
		ID:         "evt-tl-1",
		CameraName: "cam1",
		Label:      "person",
		Score:      0.95,
		Timestamp:  date.Add(10*time.Hour + 23*time.Minute),
		EndTime:    date.Add(10*time.Hour + 24*time.Minute),
	}
	if err := db.SaveEvent(ev); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/cameras/cam1/timeline?date=2026-03-25", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var body struct {
		Segments []struct{} `json:"segments"`
		Events   []struct {
			ID      string `json:"id"`
			EndTime string `json:"end_time"`
		} `json:"events"`
		Activity []struct {
			Time  string  `json:"t"`
			Score float64 `json:"s"`
		} `json:"activity"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(body.Activity) != 2 {
		t.Fatalf("got %d activity buckets, want 2", len(body.Activity))
	}
	if body.Activity[0].Score != 0.73 {
		t.Errorf("activity[0].s = %f, want 0.73", body.Activity[0].Score)
	}

	if len(body.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(body.Events))
	}
	if body.Events[0].EndTime == "" {
		t.Error("event end_time should be present")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestHandleCameraTimeline_ActivityAndEndTime -v`
Expected: FAIL — `activity` field missing from response

- [ ] **Step 3: Extend handleCameraTimeline**

In `internal/api/server.go`, modify `handleCameraTimeline`:

Add `end_time` to the `timelineEvent` struct (after line 1229):

```go
type timelineEvent struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	Score     float32   `json:"score"`
	Timestamp time.Time `json:"timestamp"`
	EndTime   time.Time `json:"end_time,omitempty"`
}
```

Set `EndTime` when building event list (line 1242):

```go
evts = append(evts, timelineEvent{
	ID:        evt.ID,
	Label:     evt.Label,
	Score:     evt.Score,
	Timestamp: evt.Timestamp,
	EndTime:   evt.EndTime,
})
```

Add a new struct for activity:

```go
type timelineActivity struct {
	Time  time.Time `json:"t"`
	Score float64   `json:"s"`
}
```

Fetch motion activity (after fetching events):

```go
activity, err := s.db.GetMotionActivity(name, date)
if err != nil {
	slog.Error("failed to get motion activity", "camera", name, "error", err)
	activity = nil
}

acts := make([]timelineActivity, 0, len(activity))
for _, a := range activity {
	acts = append(acts, timelineActivity{Time: a.Bucket, Score: a.Score})
}
```

Add to response (line 1250):

```go
writeJSON(w, http.StatusOK, map[string]any{
	"segments": segs,
	"events":   evts,
	"activity": acts,
})
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/api/ -run TestHandleCameraTimeline_ActivityAndEndTime -v`
Expected: PASS

- [ ] **Step 5: Run all API tests**

Run: `go test ./internal/api/ -v`
Expected: All PASS

- [ ] **Step 6: Commit**

```bash
git add internal/api/server.go internal/api/server_test.go
git commit -m "feat(api): add motion activity and event end_time to timeline response"
```

---

### Task 7: Frontend — Canvas HTML and CSS

**Files:**
- Modify: `internal/api/static/camera.html:147-149`
- Modify: `internal/api/static/style.css:1228-1246`

- [ ] **Step 1: Add canvas element to camera.html**

In `camera.html`, change the timeline track (line 147-149) from:

```html
<div class="timeline-track" id="timeline-track" role="slider" aria-label="Recording timeline" tabindex="0">
  <div class="timeline-playhead" id="timeline-playhead" style="left: 50%"></div>
</div>
```

To:

```html
<div class="timeline-track" id="timeline-track" role="slider" aria-label="Recording timeline" tabindex="0">
  <canvas id="timeline-canvas"></canvas>
  <div class="timeline-playhead" id="timeline-playhead" style="left: 50%"></div>
</div>
```

- [ ] **Step 2: Update CSS**

In `style.css`, find the `:root` or dark theme variables section and add (near `--amber`):

```css
--event-bar: var(--amber);
```

Also add in the light theme section:

```css
--event-bar: var(--amber);
```

Change `.timeline-track` height from 48px to 56px (line 1230):

```css
.timeline-track {
  position: relative;
  height: 56px;
  background: var(--surface-2);
  border-radius: var(--radius-sm);
  overflow: hidden;
  cursor: crosshair;
  user-select: none;
}
```

Replace `.timeline-segment` styles (lines 1239-1246) with canvas styles:

```css
#timeline-canvas {
  position: absolute;
  top: 0;
  left: 0;
  width: 100%;
  height: 100%;
  pointer-events: none;
}
```

- [ ] **Step 3: Verify the page loads**

Run the app with `make run` and open the camera page. The timeline should appear (taller, empty canvas). Existing playhead and event dots should still render.

- [ ] **Step 4: Commit**

```bash
git add internal/api/static/camera.html internal/api/static/style.css
git commit -m "feat(ui): add canvas element and CSS for waveform timeline"
```

---

### Task 8: Frontend — Waveform Rendering

**Files:**
- Modify: `internal/api/static/app.js:1069-1135` (fetchTimelineData, renderTimelineSegments)

- [ ] **Step 1: Add renderWaveform function**

In `app.js`, delete `renderTimelineSegments()` (lines 1092-1131) and replace it with `renderWaveform()`. The fallback for days without motion data is built into `renderWaveform` itself (the `else` branch renders flat bars from segment data).

Add this function after `fetchTimelineData`:

```javascript
function renderWaveform(activity, events, segments) {
  var canvas = el('timeline-canvas');
  if (!canvas) return;
  var track = el('timeline-track');

  // Handle high-DPI
  var dpr = window.devicePixelRatio || 1;
  var w = track.offsetWidth;
  var h = track.offsetHeight;
  canvas.width = w * dpr;
  canvas.height = h * dpr;
  canvas.style.width = w + 'px';
  canvas.style.height = h + 'px';

  var ctx = canvas.getContext('2d');
  ctx.scale(dpr, dpr);
  ctx.clearRect(0, 0, w, h);

  // Build 1440-element array from sparse activity data
  var scores = new Float64Array(1440);
  if (activity && activity.length > 0) {
    activity.forEach(function(a) {
      var d = new Date(a.t);
      var minute = d.getUTCHours() * 60 + d.getUTCMinutes();
      if (minute >= 0 && minute < 1440) {
        scores[minute] = a.s;
      }
    });
  } else {
    // Fallback: flat bars at 50% for recording segments
    if (segments) {
      segments.forEach(function(seg) {
        var start = new Date(seg.start_time);
        var end = new Date(seg.end_time);
        var startMin = start.getUTCHours() * 60 + start.getUTCMinutes();
        var endMin = end.getUTCHours() * 60 + end.getUTCMinutes();
        for (var m = startMin; m <= endMin && m < 1440; m++) {
          scores[m] = 0.5;
        }
      });
    }
  }

  // Populate mergedBlocks from segments for scrubbing hit-testing
  mergedBlocks = [];
  if (segments) {
    segments.forEach(function(seg) {
      var start = new Date(seg.start_time);
      var end = new Date(seg.end_time);
      var startSec = start.getHours() * 3600 + start.getMinutes() * 60 + start.getSeconds();
      var endSec = end.getHours() * 3600 + end.getMinutes() * 60 + end.getSeconds();
      if (endSec <= startSec) return;
      if (mergedBlocks.length > 0 && startSec - mergedBlocks[mergedBlocks.length - 1].end <= 60) {
        if (endSec > mergedBlocks[mergedBlocks.length - 1].end) {
          mergedBlocks[mergedBlocks.length - 1].end = endSec;
        }
      } else {
        mergedBlocks.push({ start: startSec, end: endSec });
      }
    });
  }

  // Build set of event minutes
  var eventMinutes = new Set();
  if (events) {
    events.forEach(function(evt) {
      var startTs = new Date(evt.timestamp);
      var startMin = startTs.getHours() * 60 + startTs.getMinutes();
      var endMin = startMin;
      if (evt.end_time) {
        var endTs = new Date(evt.end_time);
        endMin = endTs.getHours() * 60 + endTs.getMinutes();
      }
      for (var m = startMin; m <= endMin && m < 1440; m++) {
        eventMinutes.add(m);
      }
    });
  }

  // Read CSS colors
  var style = getComputedStyle(document.documentElement);
  var normalColor = style.getPropertyValue('--cyan-dim').trim() || '#00b8d4';
  var eventColor = style.getPropertyValue('--event-bar').trim() || '#ffab00';

  var barWidth = w / 1440;
  var midY = h / 2;
  var maxHalf = h / 2;
  var minBarHeight = maxHalf * 0.15;

  for (var i = 0; i < 1440; i++) {
    if (scores[i] <= 0) continue;

    var barH = scores[i] * maxHalf;
    if (barH < minBarHeight) barH = minBarHeight;

    ctx.fillStyle = eventMinutes.has(i) ? eventColor : normalColor;
    ctx.fillRect(i * barWidth, midY - barH, Math.max(barWidth, 0.5), barH * 2);
  }
}
```

- [ ] **Step 2: Update fetchTimelineData to use renderWaveform**

In `fetchTimelineData()` (line 1082-1086), change the `.then` handler:

```javascript
.then(function(data) {
  cachedSegments = data.segments || [];
  cachedActivity = data.activity || [];
  renderWaveform(cachedActivity, data.events || [], cachedSegments);
  renderTimelineEvents(data.events || []);
})
```

Add `var cachedActivity = [];` near the top of the file where `cachedSegments` is declared.

- [ ] **Step 3: Update periodic refresh**

Search `app.js` for any remaining calls to `renderTimelineSegments` (check the periodic refresh near line 2257 that refreshes segments every 30s). Replace with `renderWaveform(cachedActivity, cachedTimelineEvents, cachedSegments)`.

- [ ] **Step 4: Add resize handler**

In `initTimeline()`, add a debounced resize handler:

```javascript
var resizeTimer;
window.addEventListener('resize', function() {
  clearTimeout(resizeTimer);
  resizeTimer = setTimeout(function() {
    renderWaveform(cachedActivity, cachedTimelineEvents, cachedSegments);
    renderTimelineEvents(cachedTimelineEvents);
  }, 200);
});
```

Add `var cachedTimelineEvents = [];` near `cachedActivity`. In the fetch handler, add `cachedTimelineEvents = data.events || [];`.

- [ ] **Step 5: Test in browser**

Run `make run`, open camera page. Verify:
- Waveform renders for today (if motion data exists, otherwise flat fallback for segments)
- Event dots appear on top of waveform
- Bars during event periods are amber, others cyan
- Playhead, scrubbing, cursor, preview all work
- Resize the window — waveform redraws correctly

- [ ] **Step 6: Commit**

```bash
git add internal/api/static/app.js
git commit -m "feat(ui): render waveform timeline with motion activity data"
```

---

### Task 9: Full Integration Test

- [ ] **Step 1: Run all tests**

Run: `make check`
Expected: All tests pass, linter clean

- [ ] **Step 2: Run the app end-to-end**

Run: `make run`
Verify:
- Camera page loads, waveform timeline visible
- Navigate between days — waveform updates
- Click event dots — navigation to event page works
- Hover cursor shows time and preview thumbnail
- Playback scrubbing works
- Historical days without motion data show flat segment bars

- [ ] **Step 3: Final commit (if any fixups needed)**

```bash
git add <fixed-files>
git commit -m "fix: waveform timeline integration fixes"
```
