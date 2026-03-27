# HLS Playback Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the slow progressive MP4 remux playback with HLS using byte-range addressing over existing fMP4 segment files.

**Architecture:** Server generates m3u8 playlists on the fly by scanning fMP4 fragment positions with `indexFile()`. hls.js (vendored) plays the playlist on the client. Safari uses native HLS. No recording pipeline changes.

**Tech Stack:** Go (server), hls.js (client), HLS v7 with fMP4 byte-range segments

**Spec:** `docs/superpowers/specs/2026-03-27-hls-playback-design.md`

---

## File Structure

| File | Action | Responsibility |
|------|--------|---------------|
| `internal/media/mp4reader.go` | Modify | Extend `fragment` struct with `isSync`, add `GenerateHLSPlaylist()` |
| `internal/media/mp4reader_test.go` | Modify | Tests for sync detection and playlist generation |
| `internal/storage/db.go` | Modify | Add `GetSegmentByID()` method |
| `internal/storage/db_test.go` | Modify | Test for `GetSegmentByID()` |
| `internal/api/server.go` | Modify | Add m3u8 and segment-serving endpoints, remove remux |
| `internal/api/server_test.go` | Modify | Tests for new endpoints |
| `internal/api/static/hls.min.js` | Create | Vendored hls.js library |
| `internal/api/static/camera.html` | Modify | Add hls.js script tag |
| `internal/api/static/app.js` | Modify | Replace `startPlayback()` with HLS-based playback |

---

### Task 1: Extend indexFile() with sync sample detection

**Files:**
- Modify: `internal/media/mp4reader.go:214-222` (fragment struct)
- Modify: `internal/media/mp4reader.go:506-551` (indexFile trun/tfhd handlers)
- Test: `internal/media/mp4reader_test.go`

- [ ] **Step 1: Write test for sync sample detection**

Add to `internal/media/mp4reader_test.go`:

```go
func TestIndexFileDetectsSync(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.mp4")
	writeSyntheticFMP4(t, path, 10, 3000)

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	_, frags, _, err := indexFile(f)
	if err != nil {
		t.Fatal(err)
	}

	if len(frags) == 0 {
		t.Fatal("expected fragments")
	}

	// First fragment should be a sync sample (IDR)
	if !frags[0].isSync {
		t.Error("first fragment should be sync")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/media/ -run TestIndexFileDetectsSync -v`
Expected: FAIL — `frags[0].isSync` is undefined (field does not exist)

- [ ] **Step 3: Add isSync field to fragment struct and populate it**

In `internal/media/mp4reader.go`, add field to the struct:

```go
type fragment struct {
	moofOffset int64
	moofSize   int64
	mdatOffset int64
	mdatSize   int64
	decodeTime uint64
	duration   uint32
	trackID    uint32
	isSync     bool // true if fragment starts with a sync (keyframe) sample
}
```

In the `BoxTypeTfhd` handler inside `indexFile()`, capture default sample flags:

```go
case gomp4.BoxTypeTfhd():
	if currentFrag != nil {
		box, _, err := h.ReadPayload()
		if err != nil {
			return nil, err
		}
		tfhd := box.(*gomp4.Tfhd)
		currentFrag.trackID = tfhd.TrackID
		// Capture default sample flags for sync detection
		if tfhd.GetFlags()&0x20 != 0 { // default-sample-flags-present
			currentFrag.isSync = tfhd.DefaultSampleFlags&0x00010000 == 0
		} else {
			currentFrag.isSync = true // assume sync if no flags specified
		}
	}
	return nil, nil
```

In the `BoxTypeTrun` handler, override with first sample flags if present:

```go
case gomp4.BoxTypeTrun():
	if currentFrag != nil {
		box, _, err := h.ReadPayload()
		if err != nil {
			return nil, err
		}
		trun := box.(*gomp4.Trun)
		// Check first sample flags for sync detection
		trunFlags := trun.GetFlags()
		if trunFlags&0x04 != 0 && len(trun.Entries) > 0 {
			// first-sample-flags-present
			currentFrag.isSync = trun.FirstSampleFlags&0x00010000 == 0
		} else if trunFlags&0x400 != 0 && len(trun.Entries) > 0 {
			// sample-flags-present
			currentFrag.isSync = trun.Entries[0].SampleFlags&0x00010000 == 0
		}
		var totalDur uint32
		for _, e := range trun.Entries {
			totalDur += e.SampleDuration
		}
		currentFrag.duration += totalDur
	}
	return nil, nil
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/media/ -run TestIndexFileDetectsSync -v`
Expected: PASS

- [ ] **Step 5: Run full media test suite**

Run: `go test ./internal/media/ -v`
Expected: All existing tests still pass

- [ ] **Step 6: Commit**

```bash
git add internal/media/mp4reader.go internal/media/mp4reader_test.go
git commit -m "feat(media): detect sync samples in indexFile for HLS segment boundaries"
```

---

### Task 2: Add HLS playlist generator

**Files:**
- Modify: `internal/media/mp4reader.go` (add `GenerateHLSPlaylist()` after `indexFile`)
- Test: `internal/media/mp4reader_test.go`

- [ ] **Step 1: Write test for HLS playlist generation**

```go
func TestGenerateHLSPlaylist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.mp4")
	writeSyntheticFMP4(t, path, 10, 3000)

	type segmentRef struct {
		URI       string
		StartTime time.Time
		EndTime   time.Time
	}
	segments := []segmentRef{
		{URI: "/api/cameras/test/segments/1", StartTime: time.Now(), EndTime: time.Now().Add(10 * time.Minute)},
	}

	playlist, err := GenerateHLSPlaylist([]string{path}, []string{segments[0].URI}, 0)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(playlist, "#EXTM3U") {
		t.Error("missing EXTM3U header")
	}
	if !strings.Contains(playlist, "#EXT-X-VERSION:7") {
		t.Error("missing version 7")
	}
	if !strings.Contains(playlist, "#EXT-X-MAP:") {
		t.Error("missing EXT-X-MAP for init segment")
	}
	if !strings.Contains(playlist, "#EXT-X-BYTERANGE:") {
		t.Error("missing byte range entries")
	}
	if !strings.Contains(playlist, "#EXT-X-ENDLIST") {
		t.Error("missing ENDLIST")
	}
	if !strings.Contains(playlist, "#EXT-X-PLAYLIST-TYPE:VOD") {
		t.Error("missing VOD playlist type")
	}
	if !strings.Contains(playlist, segments[0].URI) {
		t.Error("missing segment URI")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/media/ -run TestGenerateHLSPlaylist -v`
Expected: FAIL — `GenerateHLSPlaylist` undefined

- [ ] **Step 3: Implement GenerateHLSPlaylist**

Add to `internal/media/mp4reader.go`:

```go
// GenerateHLSPlaylist creates an m3u8 playlist from one or more fMP4 segment files.
// Each file gets its own EXT-X-MAP (init segment). Fragments are grouped into
// HLS segments aligned to video keyframes using byte-range addressing.
// The start parameter skips fragments before that time offset (in the first file).
func GenerateHLSPlaylist(paths []string, uris []string, start time.Duration) (string, error) {
	var buf strings.Builder
	buf.WriteString("#EXTM3U\n")
	buf.WriteString("#EXT-X-VERSION:7\n")

	var maxSegDur float64

	// First pass: collect all segment entries to determine TARGETDURATION
	type hlsEntry struct {
		uri      string
		duration float64
		offset   int64
		size     int64
	}
	type hlsInit struct {
		uri  string
		size int64
	}
	var entries []hlsEntry
	var inits []int // index into entries where a new init segment starts

	for fileIdx, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("open %s: %w", path, err)
		}

		initBoxes, frags, trackTS, err := indexFile(f)
		f.Close()
		if err != nil {
			return "", fmt.Errorf("index %s: %w", path, err)
		}

		// Calculate init segment size (ftyp + moov)
		var initSize int64
		for _, loc := range initBoxes {
			if loc.offset+loc.size > initSize {
				initSize = loc.offset + loc.size
			}
		}

		uri := uris[fileIdx]

		// Find the video track timescale
		var videoTrackID uint32
		var videoTS uint32 = 90000
		for tid, ts := range trackTS {
			// Video track typically has higher timescale (90000)
			if ts >= 90000 {
				videoTrackID = tid
				videoTS = ts
				break
			}
		}
		// Fallback: pick the track with most fragments
		if videoTrackID == 0 {
			counts := make(map[uint32]int)
			for _, frag := range frags {
				counts[frag.trackID]++
			}
			var maxCount int
			for tid, c := range counts {
				if c > maxCount {
					maxCount = c
					videoTrackID = tid
					videoTS = trackTS[tid]
				}
			}
		}

		startTick := uint64(start.Seconds() * float64(videoTS))

		// Group fragments into HLS segments (keyframe to keyframe)
		// Track the byte range for each group
		var groupStart int64 = -1
		var groupEnd int64
		var groupDur float64

		needInit := true

		for _, frag := range frags {
			// Skip fragments before start time (only for first file)
			if fileIdx == 0 && frag.trackID == videoTrackID {
				if frag.decodeTime+uint64(frag.duration) <= startTick {
					continue
				}
			}

			// Start of a new HLS segment at each video keyframe
			if frag.trackID == videoTrackID && frag.isSync && groupStart >= 0 {
				// Emit previous group
				if needInit {
					inits = append(inits, len(entries))
					entries = append(entries, hlsEntry{
						uri: uri, duration: -1, // marker for init
						offset: 0, size: initSize,
					})
					needInit = false
				}
				entries = append(entries, hlsEntry{
					uri:      uri,
					duration: groupDur,
					offset:   groupStart,
					size:     groupEnd - groupStart,
				})
				if groupDur > maxSegDur {
					maxSegDur = groupDur
				}
				groupStart = frag.moofOffset
				groupEnd = frag.mdatOffset + frag.mdatSize
				groupDur = float64(frag.duration) / float64(videoTS)
			} else {
				if groupStart < 0 {
					if needInit {
						inits = append(inits, len(entries))
						entries = append(entries, hlsEntry{
							uri: uri, duration: -1,
							offset: 0, size: initSize,
						})
						needInit = false
					}
					groupStart = frag.moofOffset
				}
				endPos := frag.mdatOffset + frag.mdatSize
				if endPos > groupEnd {
					groupEnd = endPos
				}
				if frag.trackID == videoTrackID {
					groupDur += float64(frag.duration) / float64(videoTS)
				}
			}
		}

		// Emit last group
		if groupStart >= 0 && groupDur > 0 {
			entries = append(entries, hlsEntry{
				uri:      uri,
				duration: groupDur,
				offset:   groupStart,
				size:     groupEnd - groupStart,
			})
			if groupDur > maxSegDur {
				maxSegDur = groupDur
			}
		}

		start = 0 // only apply start offset to first file
	}

	// Write TARGETDURATION (must be integer, rounded up)
	targetDur := int(maxSegDur) + 1
	if targetDur < 1 {
		targetDur = 4
	}
	buf.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", targetDur))
	buf.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")

	// Write entries
	for _, e := range entries {
		if e.duration < 0 {
			// Init segment
			buf.WriteString(fmt.Sprintf("#EXT-X-MAP:URI=\"%s\",BYTERANGE=\"%d@%d\"\n", e.uri, e.size, e.offset))
		} else {
			buf.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", e.duration))
			buf.WriteString(fmt.Sprintf("#EXT-X-BYTERANGE:%d@%d\n", e.size, e.offset))
			buf.WriteString(e.uri + "\n")
		}
	}

	buf.WriteString("#EXT-X-ENDLIST\n")

	return buf.String(), nil
}
```

Also add `"strings"` to imports if not already present.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/media/ -run TestGenerateHLSPlaylist -v`
Expected: PASS

- [ ] **Step 5: Test with real segment file**

Add a test that uses a real fMP4 file from mac-mini (if available at `/tmp/test_fmp4.mp4`):

```go
func TestGenerateHLSPlaylistReal(t *testing.T) {
	const testFile = "/tmp/test_fmp4.mp4"
	if _, err := os.Stat(testFile); err != nil {
		t.Skip("real test file not available")
	}
	playlist, err := GenerateHLSPlaylist(
		[]string{testFile},
		[]string{"/api/cameras/test/segments/1"},
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(playlist[:min(len(playlist), 500)])

	lines := strings.Split(playlist, "\n")
	var segCount int
	for _, l := range lines {
		if strings.HasPrefix(l, "#EXTINF:") {
			segCount++
		}
	}
	t.Logf("HLS segments: %d", segCount)
	if segCount < 2 {
		t.Error("expected multiple HLS segments for a 10-minute recording")
	}
}
```

Run: `go test ./internal/media/ -run TestGenerateHLSPlaylistReal -v`
Expected: PASS with multiple HLS segments logged

- [ ] **Step 6: Commit**

```bash
git add internal/media/mp4reader.go internal/media/mp4reader_test.go
git commit -m "feat(media): add HLS playlist generator with byte-range fMP4 addressing"
```

---

### Task 3: Add GetSegmentByID to storage

**Files:**
- Modify: `internal/storage/db.go`
- Test: `internal/storage/db_test.go`

- [ ] **Step 1: Write test**

Add to `internal/storage/db_test.go`:

```go
func TestGetSegmentByID(t *testing.T) {
	db := newTestDB(t)

	// Insert a segment
	now := time.Now().UTC()
	mustSaveSegment(t, db, makeSegment("cam1", "/path/to/seg.mp4", now, now.Add(10*time.Minute), 1000))

	// Get all segments to find the ID
	segs, err := db.GetAllSegments("cam1")
	if err != nil || len(segs) == 0 {
		t.Fatal("no segments found")
	}

	// Fetch by ID
	got, err := db.GetSegmentByID(segs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected segment, got nil")
	}
	if got.Camera != "cam1" {
		t.Errorf("Camera = %q, want %q", got.Camera, "cam1")
	}

	// Fetch non-existent ID
	got, err = db.GetSegmentByID(99999)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Error("expected nil for non-existent ID")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/storage/ -run TestGetSegmentByID -v`
Expected: FAIL — `GetSegmentByID` undefined

- [ ] **Step 3: Implement GetSegmentByID**

Add to `internal/storage/db.go` near the other segment query methods:

```go
func (d *DB) GetSegmentByID(id int64) (*SegmentRecord, error) {
	row := d.db.QueryRow(
		`SELECT id, camera, path, start_time, end_time, size_bytes FROM segments WHERE id = ?`, id)
	var s SegmentRecord
	err := row.Scan(&s.ID, &s.Camera, &s.Path, &s.StartTime, &s.EndTime, &s.SizeBytes)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/storage/ -run TestGetSegmentByID -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/storage/db.go internal/storage/db_test.go
git commit -m "feat(storage): add GetSegmentByID for HLS segment serving"
```

---

### Task 4: Add server endpoints (m3u8 + segment serving)

**Files:**
- Modify: `internal/api/server.go` (register routes, add handlers)
- Test: `internal/api/server_test.go`

- [ ] **Step 1: Write tests for the new endpoints**

Add to `internal/api/server_test.go`:

```go
func TestHandlePlaybackM3U8_NotFound(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/cameras/unknown/playback.m3u8?start=2026-01-01T00:00:00Z", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestHandlePlaybackM3U8_MissingStart(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/cameras/test/playback.m3u8", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleSegment_NotFound(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/cameras/test/segments/99999", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/api/ -run "TestHandlePlaybackM3U8|TestHandleSegment" -v`
Expected: FAIL — routes not registered

- [ ] **Step 3: Register routes and implement handlers**

In `internal/api/server.go`, add route registrations in `registerRoutes()`:

```go
s.mux.HandleFunc("GET /api/cameras/{name}/playback.m3u8", s.handlePlaybackM3U8)
s.mux.HandleFunc("GET /api/cameras/{name}/segments/{id}", s.handleSegment)
```

Add the handlers:

```go
func (s *Server) handlePlaybackM3U8(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cam := s.cameras.GetCamera(name)
	if cam == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "camera not found"})
		return
	}

	startStr := r.URL.Query().Get("start")
	if startStr == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "start parameter required"})
		return
	}

	start, err := time.Parse(time.RFC3339, startStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid start time format"})
		return
	}

	durationSec := 600
	if ds := r.URL.Query().Get("duration"); ds != "" {
		if d, err := strconv.Atoi(ds); err == nil && d > 0 {
			durationSec = d
		}
	}
	if durationSec > 3600 {
		durationSec = 3600
	}

	end := start.Add(time.Duration(durationSec) * time.Second)
	segments, err := s.db.QuerySegments(name, start, end)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if len(segments) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no recordings found"})
		return
	}

	var paths []string
	var uris []string
	for _, seg := range segments {
		paths = append(paths, seg.Path)
		uris = append(uris, fmt.Sprintf("/api/cameras/%s/segments/%d", name, seg.ID))
	}

	offset := start.Sub(segments[0].StartTime)
	if offset < 0 {
		offset = 0
	}

	playlist, err := media.GenerateHLSPlaylist(paths, uris, offset)
	if err != nil {
		slog.Error("HLS playlist generation failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "playlist generation failed"})
		return
	}

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write([]byte(playlist))
}

func (s *Server) handleSegment(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	idStr := r.PathValue("id")

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid segment ID"})
		return
	}

	seg, err := s.db.GetSegmentByID(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if seg == nil || seg.Camera != name {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "segment not found"})
		return
	}

	http.ServeFile(w, r, seg.Path)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/api/ -run "TestHandlePlaybackM3U8|TestHandleSegment" -v`
Expected: PASS

- [ ] **Step 5: Run full test suite**

Run: `make check`
Expected: All tests pass, lint clean

- [ ] **Step 6: Commit**

```bash
git add internal/api/server.go internal/api/server_test.go
git commit -m "feat(api): add HLS m3u8 and segment-serving endpoints"
```

---

### Task 5: Vendor hls.js and add script tag

**Files:**
- Create: `internal/api/static/hls.min.js`
- Modify: `internal/api/static/camera.html`

- [ ] **Step 1: Download hls.js**

```bash
curl -L -o internal/api/static/hls.min.js "https://cdn.jsdelivr.net/npm/hls.js@1/dist/hls.min.js"
```

Verify the file is reasonable size (~250-300KB):
```bash
wc -c internal/api/static/hls.min.js
```

- [ ] **Step 2: Add script tag to camera.html**

In `internal/api/static/camera.html`, add a new script tag *before* the existing `<script src="/app.js"></script>` line (line 278):

```html
<script src="/hls.min.js"></script>
```

Do NOT duplicate the existing `app.js` script tag.

- [ ] **Step 3: Verify build still works**

Run: `make build`
Expected: Success (hls.min.js gets embedded via go:embed)

- [ ] **Step 4: Commit**

```bash
git add internal/api/static/hls.min.js internal/api/static/camera.html
git commit -m "feat(ui): vendor hls.js for recording playback"
```

---

### Task 6: Replace frontend playback with HLS

**Files:**
- Modify: `internal/api/static/app.js`

- [ ] **Step 1: Add HLS playback state variable**

At the top of app.js, near the existing playback state variables (around line 12):

```javascript
let playbackHls = null; // Hls instance for recording playback
```

- [ ] **Step 2: Add HLS cleanup function**

Before the `startPlayback` function:

```javascript
function cleanupPlaybackHls() {
  if (playbackHls) {
    playbackHls.destroy();
    playbackHls = null;
  }
}
```

- [ ] **Step 3: Replace startPlayback function**

Replace the entire `startPlayback` function (currently at line 1379) with:

```javascript
function startPlayback(timestamp) {
  var name = getCameraName();
  if (!name) return;

  var isoStr = timestamp.toISOString();
  var url = '/api/cameras/' + encodeURIComponent(name) + '/playback.m3u8?start=' + encodeURIComponent(isoStr);

  // Stop any live stream first
  if (currentStream) {
    stopStream();
  }
  cleanupPlaybackHls();

  var video = el('live-video');
  if (!video) return;

  playbackOffset = 0;
  playbackStartTime = timestamp;

  video.muted = true;
  video.playsInline = true;
  video.classList.remove('hidden');
  hide('live-snapshot');
  hide('live-mjpeg');

  // Enable audio controls for recordings
  updateMuteButton(true);

  video.ontimeupdate = function() {
    updatePlayheadForPlayback(video.currentTime);
  };

  if (video.canPlayType('application/vnd.apple.mpegurl')) {
    // Safari: native HLS
    video.src = url;
    video.autoplay = true;
    video.onerror = function() {
      toast('No recording found for this timestamp', 'error');
      updatePlayheadToNow();
      returnToLive();
    };
    video.onloadedmetadata = function() {
      video.play().catch(function() {});
    };
  } else if (typeof Hls !== 'undefined' && Hls.isSupported()) {
    // Chrome/Firefox/Edge: hls.js
    var hls = new Hls({ maxBufferLength: 30 });
    playbackHls = hls;
    hls.loadSource(url);
    hls.attachMedia(video);
    hls.on(Hls.Events.MANIFEST_PARSED, function() {
      video.play().catch(function() {});
    });
    hls.on(Hls.Events.ERROR, function(event, data) {
      if (data.fatal) {
        if (data.type === Hls.ErrorTypes.NETWORK_ERROR && data.response && data.response.code === 404) {
          toast('No recording found for this timestamp', 'error');
        } else {
          toast('Playback error', 'error');
        }
        hls.destroy();
        playbackHls = null;
        updatePlayheadToNow();
        returnToLive();
      }
    });
  } else {
    toast('HLS playback not supported in this browser', 'error');
    return;
  }

  playbackMode = true;
  updatePlaybackUI();
  toast('Playing recording from ' + timestamp.toLocaleTimeString());
}
```

- [ ] **Step 4: Remove playNextSegment function**

Delete the `playNextSegment` function entirely (it is no longer needed — hls.js handles multi-segment playlists).

Replace the `video.onended` handler. In `startPlayback`, the video's `onended` event now means the entire HLS playlist finished. Add after the hls.js setup:

```javascript
video.onended = function() {
  returnToLive();
};
```

- [ ] **Step 5: Update returnToLive to clean up HLS**

In the `returnToLive` function, add `cleanupPlaybackHls()` at the top (before video cleanup):

```javascript
function returnToLive() {
  cleanupPlaybackHls();
  // ... rest of existing cleanup
```

- [ ] **Step 6: Verify build**

Run: `make build`
Expected: Success

- [ ] **Step 7: Commit**

```bash
git add internal/api/static/app.js
git commit -m "feat(ui): replace progressive MP4 playback with HLS"
```

---

### Task 7: Remove old remux playback code

**Files:**
- Modify: `internal/api/server.go` (remove old handler, cache logic)

- [ ] **Step 1: Remove the old handlePlayback handler**

In `internal/api/server.go`:
- Remove the route registration for `GET /api/cameras/{name}/playback`
- Remove the `handlePlayback` function entirely
- Remove the `.playback-cache` directory logic

Keep the HEAD route if needed for backward compatibility, or remove it too.

- [ ] **Step 2: Run full test suite**

Run: `make check`
Expected: All tests pass. If any test references the old playback endpoint, update or remove it.

- [ ] **Step 3: Commit**

```bash
git add internal/api/server.go internal/api/server_test.go
git commit -m "refactor(api): remove progressive MP4 remux playback endpoint"
```

---

### Task 8: Deploy and test end-to-end

- [ ] **Step 1: Build and deploy**

```bash
make deploy
```

- [ ] **Step 2: Test on desktop Chrome**

Navigate to `https://vedetta.am8.nl/camera.html?name=front_door`. Click on the timeline where recordings exist. Verify:
- Playback starts within 1-2 seconds
- Video plays smoothly without lag
- LIVE button is greyed out during playback
- Audio can be unmuted
- Clicking "Return to Live" works

- [ ] **Step 3: Test on iPhone Safari**

Open the same URL on iPhone. Verify:
- Playback works with native HLS (no hls.js needed)
- Video plays inline (not fullscreen)
- Audio can be unmuted

- [ ] **Step 4: Test error cases**

Click on a part of the timeline with no recording. Verify:
- Toast shows "No recording found"
- Returns to live view

- [ ] **Step 5: Clean up old cache files on server**

```bash
ssh mac-mini 'find /Volumes/VedettaSSD -name ".playback-cache" -type d -exec rm -rf {} + 2>/dev/null'
```
