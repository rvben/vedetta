package camera

import (
	"context"
	"image"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/detect"
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
	selected := image.NewRGBA(image.Rect(0, 0, 2560, 1920))
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
	if event.Box != [4]int{0, 0, 2560, 1920} {
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
	cam.offerDoorbellFrame(buf, 32, 32, at, detections)
	buf[0] = 0
	detections[0].Box[0] = 12
	// A full analysis queue must not stall frame processing or doorbell presses.
	cam.offerDoorbellFrame(buf, 32, 32, at.Add(time.Second), detections)
	job := <-d.pending
	if job.frame.RGBAAt(0, 0).R != 200 || job.detections[0].Box[0] != 0 {
		t.Fatal("candidate referenced reused decoder data")
	}
	cam.offerDoorbellFrame(buf, 32, 32, at.Add(100*time.Millisecond), detections)
	if len(d.pending) != 0 {
		t.Fatal("sampling exceeded twice per second")
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
			cam.offerDoorbellFrame(buf, 32, 32, time.Now().Add(time.Duration(i)*time.Second), detections)
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
