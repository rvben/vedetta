package camera

import (
	"context"
	"image"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/detect"
	"github.com/rvben/vedetta/internal/media"
	"github.com/rvben/vedetta/internal/snapshot"
)

func TestPressUsesSelectedVisitorForSharedEvent(t *testing.T) {
	m := NewManagerForTest()
	events := make(chan Event, 1)
	m.events = events
	cam := NewTestCamera("door")
	current := image.NewRGBA(image.Rect(0, 0, 640, 480))
	cam.SetTestFrame(current)
	cam.doorbellSnapshots = newDoorbellSnapshots(nil, nil)
	// Keep the selected frame larger than the fallback without spending the
	// two-second freshness window encoding a 5 MP JPEG under race instrumentation.
	selected := image.NewRGBA(image.Rect(0, 0, 800, 600))
	cam.doorbellSnapshots.buffer.Add(selected, time.Now().Add(-500*time.Millisecond), []snapshot.Subject{{Box: image.Rect(400, 100, 700, 500), Confidence: 0.95, Face: true, Frontalness: 1}})
	m.RegisterForTest(cam)
	before := time.Now()
	id, ok := m.SubmitDoorbellPress("door")
	if !ok {
		t.Fatal("press rejected")
	}
	event := <-events
	if event.ID != id || event.SnapshotImage != selected {
		t.Fatal("shared doorbell event did not carry selected visitor image")
	}
	if event.Timestamp.Before(before) {
		t.Fatal("press timestamp was replaced by older photo timestamp")
	}
	if event.Box != [4]int{0, 0, 800, 600} {
		t.Fatalf("event dimensions: %v", event.Box)
	}
}

func TestPressFallsBackAfterVisitorExpires(t *testing.T) {
	cam := NewTestCamera("door")
	current := image.NewRGBA(image.Rect(0, 0, 640, 480))
	cam.SetTestFrame(current)
	cam.doorbellSnapshots = newDoorbellSnapshots(nil, nil)
	cam.doorbellSnapshots.buffer.Add(image.NewRGBA(image.Rect(0, 0, 1280, 960)), time.Now().Add(-3*time.Second), []snapshot.Subject{{Box: image.Rect(20, 20, 100, 100), Confidence: 0.9, Face: true}})
	if got, preview := cam.doorbellFrame(time.Now()); got.Bounds() != current.Bounds() || preview != nil {
		t.Fatal("stale visitor won over current frame")
	}
}

func TestDoorbellWorkerScoresActualPixelsAndStops(t *testing.T) {
	d := newDoorbellSnapshots(nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	img := image.NewRGBA(image.Rect(0, 0, 640, 480))
	at := time.Now()
	d.pending <- doorbellFrameJob{frame: img, at: at, detections: []detect.Detection{{Label: "person", Score: 0.9, Box: [4]int{100, 100, 300, 450}}}}
	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if got, _, kind := d.buffer.Best(time.Now()); got == img && kind == "person" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("worker did not score visitor")
		case <-ticker.C:
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker failed to stop")
	}
	if got, _, _ := d.buffer.Best(time.Now()); got != nil || d.active.Load() {
		t.Fatal("stopped worker retained images or stayed active")
	}
}

func TestOfferDoorbellFrameOwnsPixelsAndDoesNotBlock(t *testing.T) {
	cam := NewTestCamera("door")
	d := newDoorbellSnapshots(nil, nil)
	cam.doorbellSnapshots = d
	d.active.Store(true)
	buf := make([]byte, 32*32*3)
	buf[0] = 200
	detections := []detect.Detection{{Label: "person", Score: 0.9, Box: [4]int{0, 0, 32, 32}}}
	at := time.Now()
	cam.offerDoorbellFrame(rgbFrame(buf, 32, 32), at, detections, true)
	buf[0] = 0
	detections[0].Box[0] = 12
	// A full analysis queue must not stall frame processing or doorbell presses.
	cam.offerDoorbellFrame(rgbFrame(buf, 32, 32), at.Add(time.Second), detections, true)
	job := <-d.pending
	if job.frame.RGBAAt(0, 0).R != 200 || job.detections[0].Box[0] != 0 {
		t.Fatal("candidate referenced reused decoder data")
	}
	cam.offerDoorbellFrame(rgbFrame(buf, 32, 32), at.Add(100*time.Millisecond), detections, true)
	if len(d.pending) != 0 {
		t.Fatal("sampling exceeded twice per second")
	}
}

// signallingDecoder reports each keyframe it is asked to decode and produces
// no picture.
type signallingDecoder struct{ decodes chan struct{} }

func (d signallingDecoder) Decode([]byte) *image.YCbCr {
	d.decodes <- struct{}{}
	return nil
}
func (signallingDecoder) Flush() *image.YCbCr { return nil }
func (signallingDecoder) Close()              {}

// Asking the snapshot consumer for its frame decodes a full-resolution
// keyframe. Queuing runs on the detection loop, which must not wait for that:
// the job carries the consumer, and the worker that analyses the job asks it.
func TestDoorbellMainFrameIsDecodedByTheWorker(t *testing.T) {
	cam := NewTestCamera("door")
	d := newDoorbellSnapshots(nil, nil)
	cam.doorbellSnapshots = d
	d.active.Store(true)

	// A camera without a main stream queues its frames all the same.
	cam.offerDoorbellFrame(rgbFrame(make([]byte, 32*32*3), 32, 32), time.Now(), nil, true)
	if job := <-d.pending; job.mainSource != nil {
		t.Fatal("a camera without a main stream queued a main-frame source")
	}

	dec := signallingDecoder{decodes: make(chan struct{}, 4)}
	cam.snapConsumer = media.NewTestSnapshotConsumerWithKeyframe(dec)
	cam.offerDoorbellFrame(rgbFrame(make([]byte, 32*32*3), 32, 32), time.Now().Add(time.Second), nil, true)
	if len(d.pending) != 1 {
		t.Fatal("the frame was not queued")
	}
	if len(dec.decodes) != 0 {
		t.Fatal("queuing a frame decoded the main-stream keyframe on the detection loop")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	select {
	case <-dec.decodes:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker did not decode the main-stream keyframe of the job it analysed")
	}
}

func TestDoorbellFaceFrontalness(t *testing.T) {
	front := [5][2]float32{{10, 10}, {30, 10}, {20, 20}, {15, 30}, {25, 30}}
	side := front
	side[2][0] = 29
	if faceFrontalness(front) <= faceFrontalness(side) {
		t.Fatal("frontal face should be preferred")
	}
	if faceFrontalness([5][2]float32{}) != 0 {
		t.Fatal("invalid landmarks should not imply a frontal face")
	}
}

func TestDoorbellStopDrainsConcurrentOffers(t *testing.T) {
	cam := NewTestCamera("door")
	d := newDoorbellSnapshots(nil, nil)
	cam.doorbellSnapshots = d
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.run(ctx); close(done) }()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !d.active.Load() {
		select {
		case <-ticker.C:
		case <-deadline:
			cancel()
			t.Fatal("worker did not start")
		}
	}
	buf := make([]byte, 32*32*3)
	detections := []detect.Detection{{Label: "person", Score: 0.9, Box: [4]int{0, 0, 32, 32}}}
	offersDone := make(chan struct{})
	go func() {
		defer close(offersDone)
		for i := 0; i < 100; i++ {
			cam.offerDoorbellFrame(rgbFrame(buf, 32, 32), time.Now().Add(time.Duration(i)*time.Second), detections, true)
		}
	}()
	cancel()
	<-done
	<-offersDone
	if len(d.pending) != 0 {
		t.Fatal("camera stop retained a queued image")
	}
	if frame, _, _ := d.buffer.Best(time.Now()); frame != nil {
		t.Fatal("camera stop retained a selected image")
	}
}

func TestDoorbellWorkerExpiresImagesWithoutNewFrames(t *testing.T) {
	d := newDoorbellSnapshots(nil, nil)
	old := time.Now().Add(-3 * time.Second)
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	d.buffer.Add(img, old, []snapshot.Subject{{Box: img.Bounds(), Confidence: 0.9}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		// Query at the OLD frame time, so Best itself cannot expire the image.
		// No new jobs arrive: only the background expiry ticker can release it.
		if got, _, _ := d.buffer.Best(old); got == nil {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatal("offline camera retained old visitor photo")
		}
	}
}

// A still scene has no visitor to photograph, so its frames are not queued for
// analysis. Motion, or a person in the frame, queues them again.
func TestOfferDoorbellFrameSkipsAQuietScene(t *testing.T) {
	person := []detect.Detection{{Label: "person", Score: 0.9, Box: [4]int{4, 4, 28, 28}}}
	car := []detect.Detection{{Label: "car", Score: 0.9, Box: [4]int{4, 4, 28, 28}}}
	cases := []struct {
		name        string
		detections  []detect.Detection
		sceneActive bool
		wantQueued  bool
	}{
		{"quiet and empty", nil, false, false},
		{"quiet with a car", car, false, false},
		{"quiet with a person", person, false, true},
		{"recent motion and empty", nil, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cam := NewTestCamera("door")
			cam.doorbellSnapshots = newDoorbellSnapshots(nil, nil)
			cam.doorbellSnapshots.active.Store(true)
			at := time.Now()
			cam.offerDoorbellFrame(rgbFrame(make([]byte, 32*32*3), 32, 32), at, tc.detections, tc.sceneActive)
			if got := len(cam.doorbellSnapshots.pending) == 1; got != tc.wantQueued {
				t.Fatalf("queued = %v, want %v", got, tc.wantQueued)
			}
			// A skipped frame must not start the sampling interval, or the first
			// frame after motion begins would be dropped as too soon.
			cam.offerDoorbellFrame(rgbFrame(make([]byte, 32*32*3), 32, 32), at.Add(time.Millisecond), person, true)
			if len(cam.doorbellSnapshots.pending) != 1 {
				t.Fatal("the first frame of an active scene was not queued")
			}
		})
	}
}

func TestDoorbellSceneActive(t *testing.T) {
	now := time.Now()
	cam := NewTestCamera("door")
	cam.detectEnabled = true
	if cam.doorbellSceneActive(now) {
		t.Fatal("a camera that never saw motion counts as active")
	}
	cam.lastMotion = now.Add(-29 * time.Second)
	if !cam.doorbellSceneActive(now) {
		t.Fatal("motion inside the hold does not count as active")
	}
	cam.lastMotion = now.Add(-30 * time.Second)
	if cam.doorbellSceneActive(now) {
		t.Fatal("motion older than the hold still counts as active")
	}
	// Without detection there is no motion signal, so every frame is a candidate.
	cam.detectEnabled = false
	if !cam.doorbellSceneActive(now) {
		t.Fatal("a camera without detection never analyses a frame")
	}
}
