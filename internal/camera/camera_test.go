package camera

import (
	"context"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/detect"
	"github.com/rvben/vedetta/internal/media"
	"github.com/rvben/vedetta/internal/metrics"
	"github.com/rvben/vedetta/internal/rtsp"
)

// processFrame must record a frames-processed count and a motion-detect latency
// observation for every frame that reaches the detection pipeline, labelled by
// camera. YOLO inference must only be timed when motion qualifies, so with no
// qualified motion the inference histogram stays empty.
func TestProcessFrameRecordsDetectionMetrics(t *testing.T) {
	metrics.ResetForTest()
	t.Cleanup(metrics.ResetForTest)

	cam := newTestCamera(config.CameraConfig{
		Name:   "frontdoor",
		Detect: config.DetectStreamConfig{Width: 16, Height: 16, FPS: 5},
	}, nil)
	// Force no qualified motion so the (nil) object detector is never invoked;
	// this isolates the motion-path recording points.
	cam.motionMinRegionScore = 1e9

	w, h := 16, 16
	cam.processFrame(media.NewTestRawFrame(w, h, 0))

	var b strings.Builder
	metrics.WriteProm(&b)
	out := b.String()

	if !strings.Contains(out, `vedetta_frames_processed_total{camera="frontdoor"} 1`) {
		t.Errorf("frames_processed_total not recorded:\n%s", out)
	}
	if !strings.Contains(out, `vedetta_motion_detect_duration_seconds_count{camera="frontdoor"} 1`) {
		t.Errorf("motion_detect_duration_seconds not recorded:\n%s", out)
	}
	if strings.Contains(out, `vedetta_yolo_inference_duration_seconds_count{camera="frontdoor"}`) {
		t.Errorf("yolo inference recorded without qualified motion:\n%s", out)
	}
}

func newTestCamera(cfg config.CameraConfig, hub *rtsp.Hub) *Camera {
	return NewCamera(
		cfg,
		nil,
		config.MotionConfig{PixelThreshold: 25, MinArea: 200, BackgroundAlpha: 0.05, MinRegionScore: 0.02},
		make(chan Event, 1),
		make(chan EventEnd, 1),
		nil,
		hub,
		"",
		85,
		"",
		nil,
		nil,
		"",
		nil,
		nil,
	)
}

func TestSnapshotRGB24_NoFrame(t *testing.T) {
	cam := newTestCamera(config.CameraConfig{
		Name:   "test",
		Detect: config.DetectStreamConfig{Width: 64, Height: 64, FPS: 5},
	}, nil)

	dst := make([]byte, 64*64*3)
	_, _, ok := cam.SnapshotRGB24(dst)
	if ok {
		t.Fatal("expected ok=false when no frame available")
	}
}

func TestSnapshotRGB24_CopiesFrame(t *testing.T) {
	cam := newTestCamera(config.CameraConfig{
		Name:   "test",
		Detect: config.DetectStreamConfig{Width: 4, Height: 4, FPS: 5},
	}, nil)

	frameSize := 4 * 4 * 3
	frame := make([]byte, frameSize)
	for i := range frame {
		frame[i] = byte(i % 256)
	}

	cam.mu.Lock()
	cam.rawFrame = make([]byte, frameSize)
	copy(cam.rawFrame, frame)
	cam.frameW = 4
	cam.frameH = 4
	cam.mu.Unlock()

	dst := make([]byte, frameSize)
	w, h, ok := cam.SnapshotRGB24(dst)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if w != 4 || h != 4 {
		t.Fatalf("expected 4x4, got %dx%d", w, h)
	}
	for i := range frame {
		if dst[i] != frame[i] {
			t.Fatalf("byte %d: got %d, want %d", i, dst[i], frame[i])
		}
	}
}

func TestSnapshotRGB24_DstTooSmall(t *testing.T) {
	cam := newTestCamera(config.CameraConfig{
		Name:   "test",
		Detect: config.DetectStreamConfig{Width: 4, Height: 4, FPS: 5},
	}, nil)

	frameSize := 4 * 4 * 3
	cam.mu.Lock()
	cam.rawFrame = make([]byte, frameSize)
	cam.frameW = 4
	cam.frameH = 4
	cam.mu.Unlock()

	dst := make([]byte, 10) // too small
	_, _, ok := cam.SnapshotRGB24(dst)
	if ok {
		t.Fatal("expected ok=false when dst too small")
	}
}

// A FIFO with no writer makes os.Open block in the open syscall
// indefinitely - the same unbounded hang a stalled recordings volume
// produces in production. loadCachedSnapshot ran synchronously inside
// Camera.Start, which Manager.Start calls synchronously inside
// initSubsystems before the API is marked ready. One camera's snapshot
// path on a stalled disk therefore bricked the entire NVR at HTTP 503.
func blockingSnapshotFIFO(t *testing.T) string {
	t.Helper()
	fifo := filepath.Join(t.TempDir(), "latest.jpg")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	return fifo
}

func TestStartReturnsPromptlyWhenCachedSnapshotReadBlocks(t *testing.T) {
	cam := newTestCamera(config.CameraConfig{Name: "test", URL: "rtsp://localhost/test"}, nil)
	cam.latestSnapshotPath = blockingSnapshotFIFO(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		cam.Start(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Camera.Start did not return while the cached snapshot read blocked: NVR readiness is gated on unbounded disk I/O")
	}
}

func TestLoadCachedSnapshotIsBoundedWhenReadBlocks(t *testing.T) {
	cam := newTestCamera(config.CameraConfig{Name: "test", URL: "rtsp://localhost/test"}, nil)
	cam.latestSnapshotPath = blockingSnapshotFIFO(t)

	done := make(chan struct{})
	go func() {
		cam.loadCachedSnapshot()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(cachedSnapshotLoadTimeout + 2*time.Second):
		t.Fatal("loadCachedSnapshot did not return within its timeout while the read blocked")
	}
}

func TestFrameSize(t *testing.T) {
	cam := newTestCamera(config.CameraConfig{
		Name:   "test",
		Detect: config.DetectStreamConfig{Width: 320, Height: 240, FPS: 5},
	}, nil)

	expected := 320 * 240 * 3
	if got := cam.FrameSize(); got != expected {
		t.Fatalf("FrameSize() = %d, want %d", got, expected)
	}
}

func TestIsOnline_NoHub(t *testing.T) {
	cam := newTestCamera(config.CameraConfig{
		Name: "test",
		URL:  "rtsp://localhost/test",
	}, nil)

	if cam.IsOnline() {
		t.Error("expected IsOnline=false with nil hub")
	}
}

func TestIsOnline_NoSource(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := rtsp.NewHub(ctx)
	defer hub.Close()

	cam := newTestCamera(config.CameraConfig{
		Name: "test",
		URL:  "rtsp://localhost/test",
	}, hub)

	// No source created for this URL yet
	if cam.IsOnline() {
		t.Error("expected IsOnline=false when no source exists")
	}
}

func TestCameraReconnects_AccumulatesFromSources(t *testing.T) {
	// Cancel the context before creating sources so the hub's per-source
	// connect goroutines exit immediately and never touch the counter.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	hub := rtsp.NewHub(ctx)
	defer hub.Close()

	cam := newTestCamera(config.CameraConfig{
		Name:      "front",
		URL:       "rtsp://localhost/detect",
		RecordURL: "rtsp://localhost/record",
	}, hub)

	// newTestCamera -> NewCamera already registered both stream URLs' reconnect
	// sinks with the hub. Create the sources directly here since the real ones
	// need a live RTSP server; the hub wires the registered sinks on creation.
	detectSrc := hub.GetOrCreate(cam.DetectURL())
	recordSrc := hub.GetOrCreate(cam.RecordURL())

	for range 2 {
		detectSrc.SimulateReconnectForTest()
	}
	for range 3 {
		recordSrc.SimulateReconnectForTest()
	}

	if got := cam.Reconnects(); got != 5 {
		t.Errorf("Reconnects() = %d, want 5 (2 detect + 3 record)", got)
	}
}

func TestCameraReconnects_Fresh(t *testing.T) {
	cam := newTestCamera(config.CameraConfig{Name: "x", URL: "rtsp://localhost/x"}, nil)
	if got := cam.Reconnects(); got != 0 {
		t.Errorf("Reconnects() = %d, want 0 for a fresh camera", got)
	}
}

// The reconnect count must stay monotonic across a stop/start: stopping a
// camera removes its RTSP source from the hub (a fresh Source with a zero
// counter is created on restart), so the cumulative total has to live on the
// long-lived Camera, not the ephemeral Source.
func TestCameraReconnects_SurvivesSourceRemoval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	hub := rtsp.NewHub(ctx)
	defer hub.Close()

	cam := newTestCamera(config.CameraConfig{Name: "front", URL: "rtsp://localhost/only"}, hub)

	// newTestCamera -> NewCamera registered the sink once. The registry, not the
	// ephemeral source, is what keeps the count monotonic across a stop/start.
	src := hub.GetOrCreate(cam.RecordURL())
	for range 4 {
		src.SimulateReconnectForTest()
	}
	if got := cam.Reconnects(); got != 4 {
		t.Fatalf("Reconnects() = %d, want 4 before removal", got)
	}

	// Stop: the source (and its own counter) is discarded.
	hub.Remove(cam.RecordURL())
	if got := cam.Reconnects(); got != 4 {
		t.Errorf("Reconnects() = %d, want 4 after source removal (must not reset)", got)
	}

	// Restart: a fresh source picks up the registered sink automatically (no
	// re-registration) and keeps accumulating.
	src2 := hub.GetOrCreate(cam.RecordURL())
	src2.SimulateReconnectForTest()
	if got := cam.Reconnects(); got != 5 {
		t.Errorf("Reconnects() = %d, want 5 after restart + 1 drop", got)
	}
}

func TestStatus_NoHub(t *testing.T) {
	cam := newTestCamera(config.CameraConfig{
		Name: "test",
		URL:  "rtsp://localhost/test",
	}, nil)

	st := cam.Status()
	if st.Online {
		t.Error("expected Online=false with nil hub")
	}
	if st.Name != "test" {
		t.Errorf("Name = %q, want %q", st.Name, "test")
	}
}

// Online state must reflect whether frames are actually flowing — not the raw
// RTSP source connection flag. The flag can be stale after reconnects, leaving
// MQTT/HA/Prometheus reporting a healthy camera as offline. lastFrameTime
// freshness is the signal users actually observe (snapshots succeeding,
// dashboard images updating).

func TestStatus_OnlineWhenFrameIsFresh(t *testing.T) {
	cam := newTestCamera(config.CameraConfig{Name: "test"}, nil)
	cam.SetTestLastFrameTime(time.Now())

	if st := cam.Status(); !st.Online {
		t.Error("expected Online=true with a fresh frame")
	}
}

func TestStatus_OfflineWhenFrameIsStale(t *testing.T) {
	cam := newTestCamera(config.CameraConfig{Name: "test"}, nil)
	cam.SetTestLastFrameTime(time.Now().Add(-30 * time.Second))

	if st := cam.Status(); st.Online {
		t.Error("expected Online=false when last frame is older than the freshness window")
	}
}

func TestStatus_OfflineWhenNoFrameYet(t *testing.T) {
	cam := newTestCamera(config.CameraConfig{Name: "test"}, nil)

	if st := cam.Status(); st.Online {
		t.Error("expected Online=false when no frame has ever arrived")
	}
}

func TestIsOnline_FreshFrameWithoutHub(t *testing.T) {
	cam := newTestCamera(config.CameraConfig{Name: "test"}, nil)
	cam.SetTestLastFrameTime(time.Now())

	if !cam.IsOnline() {
		t.Error("expected IsOnline=true when last frame is fresh, regardless of hub state")
	}
}

func TestIsOnline_StaleFrame(t *testing.T) {
	cam := newTestCamera(config.CameraConfig{Name: "test"}, nil)
	cam.SetTestLastFrameTime(time.Now().Add(-30 * time.Second))

	if cam.IsOnline() {
		t.Error("expected IsOnline=false when last frame is stale")
	}
}

func TestManager_AddCamera(t *testing.T) {
	events := make(chan Event, 10)
	eventEnds := make(chan EventEnd, 10)
	presenceEvents := make(chan PresenceEvent, 10)
	faceEvents := make(chan FaceEvent, 10)
	m := NewManager(nil, nil, config.MotionConfig{}, events, eventEnds, presenceEvents, nil, "", 85, "", nil, faceEvents, "", nil, nil)
	if len(m.ListCameras()) != 0 {
		t.Fatal("expected 0 cameras initially")
	}
	cfg := config.CameraConfig{Name: "test_cam", URL: "rtsp://localhost/stream"}
	m.AddCamera(cfg)
	names := m.ListCameras()
	if len(names) != 1 || names[0] != "test_cam" {
		t.Errorf("expected [test_cam], got %v", names)
	}
	// Adding same name again should be a no-op
	m.AddCamera(cfg)
	if len(m.ListCameras()) != 1 {
		t.Error("duplicate add should be ignored")
	}
}

func TestProcessFrame_PreservesDetectorDegradedState(t *testing.T) {
	cam := newTestCamera(config.CameraConfig{
		Name:   "test",
		URL:    "rtsp://localhost/test",
		Detect: config.DetectStreamConfig{Width: 4, Height: 4, FPS: 5},
	}, nil)

	if st := cam.Status(); !st.Degraded || st.DegradedReason != "object detector unavailable" {
		t.Fatalf("initial status = %+v, want degraded object detector unavailable", st)
	}

	cam.processFrame(media.NewTestRawFrame(4, 4, 0))

	if st := cam.Status(); !st.Degraded || st.DegradedReason != "object detector unavailable" {
		t.Fatalf("status after frame = %+v, want degraded object detector unavailable", st)
	}
}

// TestRunTrackingPipeline_TrackDecaysAcrossQuietPeriods is the regression test
// for the "no events" production bug.
//
// Symptom: after hours of normal operation, the camera stopped emitting events
// (and presence transitions stalled, so MQTT/HA went silent) until restart.
//
// Root cause: tracker.Update was gated behind motion. During quiet periods
// nothing aged the tracks, so they froze with their last position. When motion
// resumed, the IoU greedy matcher absorbed each fresh detection into one of
// the stale tracks (any nonzero overlap is accepted, regardless of label).
// The camera's confirmedTracks map still held the old eventID for that
// TrackID, so the "is this a new track?" check skipped emission. The pipeline
// went silent without any error log.
//
// The fix is to call runTrackingPipeline every frame (with detections=nil
// during quiet periods) so unmatched tracks accumulate disappeared counters
// and eventually decay. This test asserts the post-fix behavior across a full
// motion → quiet → motion cycle and would have caught the bug before it
// shipped.
func TestRunTrackingPipeline_TrackDecaysAcrossQuietPeriods(t *testing.T) {
	events := make(chan Event, 16)
	eventEnds := make(chan EventEnd, 16)
	presenceEvents := make(chan PresenceEvent, 4)

	cam := NewCamera(
		config.CameraConfig{
			Name:   "test",
			URL:    "rtsp://localhost/test",
			Detect: config.DetectStreamConfig{Width: 100, Height: 100, FPS: 5},
		},
		nil,
		config.MotionConfig{},
		events,
		eventEnds,
		presenceEvents,
		nil,
		"", // no event snapshot dir → skip snapshot capture
		85,
		"",
		nil,
		nil,
		"",
		nil,
		nil,
	)

	car := []detect.Detection{{Label: "car", Score: 0.9, Box: [4]int{10, 10, 50, 50}}}

	// Default tracker is NewTracker(30, 3): confirm after 3 hits, decay after
	// 30 misses. Drive 3 detection frames to confirm and emit the first event.
	for i := 0; i < 3; i++ {
		cam.runTrackingPipeline(car, rgbFrame(nil, 100, 100))
	}

	var firstEv Event
	select {
	case firstEv = <-events:
	default:
		t.Fatal("expected an event after 3 confirmation frames")
	}
	if firstEv.Label != "car" {
		t.Errorf("first event label = %q, want %q", firstEv.Label, "car")
	}

	// Quiet period: more than maxDisappeared empty frames so the track decays
	// and DeletedTracks() reports it. With the bug this loop is a no-op
	// (the tracker is never updated), the EventEnd never fires, and the
	// confirmedTracks entry persists across the gap.
	for i := 0; i < 32; i++ {
		cam.runTrackingPipeline(nil, rgbFrame(nil, 100, 100))
	}

	var endEv EventEnd
	select {
	case endEv = <-eventEnds:
	default:
		t.Fatal("expected EventEnd after 32 quiet frames — track did not decay (regression: tracker.Update was not invoked during quiet period)")
	}
	if endEv.EventID != firstEv.ID {
		t.Errorf("EventEnd.EventID = %q, want %q (mismatched event lifecycle)", endEv.EventID, firstEv.ID)
	}
	if endEv.CameraName != "test" {
		t.Errorf("EventEnd.CameraName = %q, want %q", endEv.CameraName, "test")
	}

	// Motion resumes with a detection at the SAME position. Because the
	// previous track was deleted and its TrackID retired, this must produce a
	// fresh track and a fresh event. Under the bug, IoU matching would map
	// the new detection onto the old (stale) track and the confirmedTracks
	// lookup would suppress emission entirely — i.e., events <- nothing.
	for i := 0; i < 3; i++ {
		cam.runTrackingPipeline(car, rgbFrame(nil, 100, 100))
	}

	var secondEv Event
	select {
	case secondEv = <-events:
	default:
		t.Fatal("expected a NEW event after motion resumed — production bug suppressed this exact case")
	}
	if secondEv.ID == firstEv.ID {
		t.Errorf("second event reused the first event's ID %q — track was not retired", secondEv.ID)
	}
	if secondEv.Label != "car" {
		t.Errorf("second event label = %q, want %q", secondEv.Label, "car")
	}

	// And the event channel should be drained — only one new event per
	// confirmation cycle. A spurious extra event would indicate double emission.
	select {
	case extra := <-events:
		t.Errorf("unexpected extra event after motion resumed: %+v", extra)
	default:
	}
}

// TestRunTrackingPipeline_NoEventWithoutMotion makes sure the fix doesn't
// over-correct: calling the pipeline every frame with empty detections must
// not synthesize events out of nothing.
func TestRunTrackingPipeline_NoEventWithoutMotion(t *testing.T) {
	events := make(chan Event, 4)
	eventEnds := make(chan EventEnd, 4)

	cam := NewCamera(
		config.CameraConfig{
			Name:   "test",
			URL:    "rtsp://localhost/test",
			Detect: config.DetectStreamConfig{Width: 100, Height: 100, FPS: 5},
		},
		nil,
		config.MotionConfig{},
		events,
		eventEnds,
		make(chan PresenceEvent, 1),
		nil, "", 85, "", nil, nil, "", nil, nil,
	)

	for i := 0; i < 50; i++ {
		cam.runTrackingPipeline(nil, rgbFrame(nil, 100, 100))
	}

	select {
	case ev := <-events:
		t.Errorf("got unexpected event with no detections ever: %+v", ev)
	default:
	}
	select {
	case end := <-eventEnds:
		t.Errorf("got unexpected EventEnd with no events ever: %+v", end)
	default:
	}
}

// TestProcessFrame_DrivesPipelineDuringQuietFrames is a higher-level regression
// guard: even if the runTrackingPipeline contract holds, the bug also requires
// that processFrame call into it on every frame (not just motion-qualifying
// ones). This test primes a confirmed track and then runs processFrame with
// frames guaranteed not to qualify (MinRegionScore=2.0 with region scores
// bounded at 1.0). EventEnd must still fire — proving processFrame keeps
// driving the pipeline even when no motion is detected.
func TestProcessFrame_DrivesPipelineDuringQuietFrames(t *testing.T) {
	events := make(chan Event, 16)
	eventEnds := make(chan EventEnd, 16)

	cam := NewCamera(
		config.CameraConfig{
			Name:   "test",
			URL:    "rtsp://localhost/test",
			Detect: config.DetectStreamConfig{Width: 32, Height: 32, FPS: 5},
		},
		nil,
		// MinRegionScore=2.0 guarantees motion never qualifies (region.Score ≤ 1.0).
		// This keeps the nil detector from being dereferenced in processFrame.
		config.MotionConfig{PixelThreshold: 25, MinArea: 200, BackgroundAlpha: 0.05, MinRegionScore: 2.0},
		events, eventEnds, make(chan PresenceEvent, 1),
		nil, "", 85, "", nil, nil, "", nil, nil,
	)

	car := []detect.Detection{{Label: "car", Score: 0.9, Box: [4]int{1, 1, 10, 10}}}
	for i := 0; i < 3; i++ {
		cam.runTrackingPipeline(car, rgbFrame(nil, 32, 32))
	}
	select {
	case <-events:
	default:
		t.Fatal("setup: expected event after confirming track")
	}

	black := media.NewTestRawFrame(32, 32, 0)
	for i := 0; i < 40; i++ { // 40 > tracker maxDisappeared (30)
		cam.processFrame(black)
	}

	select {
	case <-eventEnds:
	default:
		t.Fatal("expected EventEnd after non-motion frames — processFrame did not drive the tracking pipeline (regression)")
	}
}

// TestRunTrackingPipeline_StableTrackEmitsExactlyOnce verifies that a track
// which keeps being detected (continuous motion) produces exactly one event
// across many frames — i.e., the new always-on Update call doesn't cause
// re-emission for the same TrackID.
func TestRunTrackingPipeline_StableTrackEmitsExactlyOnce(t *testing.T) {
	events := make(chan Event, 16)
	eventEnds := make(chan EventEnd, 16)

	cam := NewCamera(
		config.CameraConfig{
			Name:   "test",
			URL:    "rtsp://localhost/test",
			Detect: config.DetectStreamConfig{Width: 100, Height: 100, FPS: 5},
		},
		nil,
		config.MotionConfig{},
		events,
		eventEnds,
		make(chan PresenceEvent, 1),
		nil, "", 85, "", nil, nil, "", nil, nil,
	)

	person := []detect.Detection{{Label: "person", Score: 0.9, Box: [4]int{30, 30, 70, 70}}}
	for i := 0; i < 50; i++ {
		cam.runTrackingPipeline(person, rgbFrame(nil, 100, 100))
	}

	count := 0
	for {
		select {
		case <-events:
			count++
			continue
		default:
		}
		break
	}
	if count != 1 {
		t.Errorf("expected exactly 1 event across 50 stable-detection frames, got %d", count)
	}

	select {
	case end := <-eventEnds:
		t.Errorf("got unexpected EventEnd while track was still alive: %+v", end)
	default:
	}
}

// TestSetTrackName_PropagatesToBroadcast asserts that a name pushed via
// SetTrackName appears on subsequent broadcastDetections frames for the
// same TrackID. This is the wire that lets re-IDed objects light up on the
// live overlay without the client polling separately.
func TestSetTrackName_PropagatesToBroadcast(t *testing.T) {
	events := make(chan Event, 16)
	eventEnds := make(chan EventEnd, 16)
	presenceEvents := make(chan PresenceEvent, 4)
	detections := make(chan DetectionFrame, 16)

	cam := NewCamera(
		config.CameraConfig{
			Name:   "test",
			URL:    "rtsp://localhost/test",
			Detect: config.DetectStreamConfig{Width: 100, Height: 100, FPS: 5},
		},
		nil,
		config.MotionConfig{},
		events, eventEnds, presenceEvents,
		nil, "", 85, "", nil, nil, "", nil, detections,
	)

	car := []detect.Detection{{Label: "car", Score: 0.9, Box: [4]int{10, 10, 50, 50}}}

	// Drive 3 frames to confirm the track and emit the first event.
	for i := 0; i < 3; i++ {
		cam.runTrackingPipeline(car, rgbFrame(nil, 100, 100))
	}
	var ev Event
	select {
	case ev = <-events:
	default:
		t.Fatal("expected first event after 3 confirmation frames")
	}
	if ev.TrackID == 0 {
		t.Fatal("event TrackID should be set so callers can SetTrackName(id, name)")
	}

	// Before naming: drain detection frames and assert no Name set.
	drainDetections(detections)
	cam.runTrackingPipeline(car, rgbFrame(nil, 100, 100))
	pre := mustReceiveDetectionFrame(t, detections)
	if len(pre.Boxes) != 1 || pre.Boxes[0].Name != "" {
		t.Fatalf("pre-name detection box should have empty Name, got %+v", pre.Boxes)
	}

	// Push a name and assert the next broadcast carries it.
	cam.SetTrackName(ev.TrackID, "Renault Trafic")
	cam.runTrackingPipeline(car, rgbFrame(nil, 100, 100))
	post := mustReceiveDetectionFrame(t, detections)
	if len(post.Boxes) != 1 {
		t.Fatalf("expected 1 box, got %d", len(post.Boxes))
	}
	if post.Boxes[0].Name != "Renault Trafic" {
		t.Errorf("post-name detection box Name = %q, want %q", post.Boxes[0].Name, "Renault Trafic")
	}
	if post.Boxes[0].TrackID != ev.TrackID {
		t.Errorf("TrackID changed across name set: was %d, now %d", ev.TrackID, post.Boxes[0].TrackID)
	}
}

// TestSetTrackName_ClearedOnTrackDelete pins the cleanup invariant: once a
// track has decayed, its entry in trackNames must be gone. TrackIDs are
// monotonic in the current tracker so collisions are impossible, but
// asserting the cleanup prevents stale names from ever leaking onto a
// future track that happens to reuse the same ID.
func TestSetTrackName_ClearedOnTrackDelete(t *testing.T) {
	events := make(chan Event, 16)
	eventEnds := make(chan EventEnd, 16)
	presenceEvents := make(chan PresenceEvent, 4)
	detections := make(chan DetectionFrame, 16)

	cam := NewCamera(
		config.CameraConfig{
			Name:   "test",
			URL:    "rtsp://localhost/test",
			Detect: config.DetectStreamConfig{Width: 100, Height: 100, FPS: 5},
		},
		nil,
		config.MotionConfig{},
		events, eventEnds, presenceEvents,
		nil, "", 85, "", nil, nil, "", nil, detections,
	)

	car := []detect.Detection{{Label: "car", Score: 0.9, Box: [4]int{10, 10, 50, 50}}}

	for i := 0; i < 3; i++ {
		cam.runTrackingPipeline(car, rgbFrame(nil, 100, 100))
	}
	var ev Event
	select {
	case ev = <-events:
	default:
		t.Fatal("expected first event")
	}
	cam.SetTrackName(ev.TrackID, "Renault Trafic")

	// Decay the track.
	for i := 0; i < 32; i++ {
		cam.runTrackingPipeline(nil, rgbFrame(nil, 100, 100))
	}

	cam.mu.RLock()
	_, stillThere := cam.trackNames[ev.TrackID]
	cam.mu.RUnlock()
	if stillThere {
		t.Errorf("trackNames[%d] should be cleared after track delete", ev.TrackID)
	}
}

// TestSetTrackName_EmptyIsNoOp documents that SetTrackName ignores empty
// names so callers can pass a re-ID match result without guarding the call
// site for the no-match case.
func TestSetTrackName_EmptyIsNoOp(t *testing.T) {
	cam := newTestCamera(config.CameraConfig{
		Name:   "test",
		Detect: config.DetectStreamConfig{Width: 4, Height: 4, FPS: 5},
	}, nil)
	cam.SetTrackName(42, "")
	cam.mu.RLock()
	_, present := cam.trackNames[42]
	cam.mu.RUnlock()
	if present {
		t.Fatal("SetTrackName(_, \"\") must be a no-op")
	}
}

func drainDetections(ch chan DetectionFrame) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func mustReceiveDetectionFrame(t *testing.T, ch chan DetectionFrame) DetectionFrame {
	t.Helper()
	select {
	case f := <-ch:
		return f
	default:
		t.Fatal("expected a detection frame on the channel")
	}
	return DetectionFrame{}
}
