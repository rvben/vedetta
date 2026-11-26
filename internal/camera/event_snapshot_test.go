package camera

import (
	"image"
	"runtime"
	"testing"

	"github.com/rvben/vedetta/internal/detect"
	"github.com/rvben/vedetta/internal/media"
)

// TestRunTrackingPipeline_OutOfZoneTrackDoesNotCopyFrameEveryFrame proves that a
// tracked object sitting outside every configured zone never triggers a
// per-frame full-resolution snapshot copy.
//
// Such a track can never be confirmed into an event (the zone gate skips it), so
// eagerly copying the full-resolution frame for it on every frame is pure waste.
// Because the object lingers indefinitely, the copy repeats every frame and ramps
// the Go runtime's heap footprint at gigabytes per minute until the memory guard
// restarts the process. The snapshot copy must be deferred until a track actually
// qualifies for an event.
func TestRunTrackingPipeline_OutOfZoneTrackDoesNotCopyFrameEveryFrame(t *testing.T) {
	const (
		detW       = 640 // detection-stream resolution
		detH       = 480
		fullW      = 1920 // full-resolution snapshot (main stream)
		fullH      = 1080
		iterations = 200
	)

	cam := NewTestCamera("zonecam")
	cam.tracker = detect.NewTracker(30, 3)
	cam.eventSnapDir = t.TempDir()
	// A zone in the bottom-right corner. The detection below sits in the
	// top-left, so it matches no zone and can never be confirmed into an event.
	cam.zones = []Zone{makeTestZone("corner", 0.6, 0.6, 0.95, 0.95, nil)}
	// Full-resolution main-stream frame: this is the buffer the production bug
	// copies twice (clean + annotated) on every frame.
	cam.snapConsumer = media.NewTestSnapshotConsumer(image.NewRGBA(image.Rect(0, 0, fullW, fullH)))

	buf := make([]byte, detW*detH*3)
	dets := []detect.Detection{{
		Label: "person",
		Score: 0.9,
		Box:   [4]int{20, 20, 120, 120}, // bottom-center anchor ~ (0.11, 0.25)
	}}

	// Sanity: the detection really is outside every configured zone.
	if matched := MatchZones(cam.zones, dets[0].Box, dets[0].Label, detW, detH); len(matched) != 0 {
		t.Fatalf("test setup wrong: detection should match no zone, matched %d", len(matched))
	}

	// Warm the tracker past minHits so the out-of-zone object is a confirmed,
	// returned track for the whole measurement window.
	for range 5 {
		cam.runTrackingPipeline(dets, rgbFrame(buf, detW, detH))
	}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range iterations {
		cam.runTrackingPipeline(dets, rgbFrame(buf, detW, detH))
	}
	runtime.ReadMemStats(&after)

	allocated := after.TotalAlloc - before.TotalAlloc

	// A single full-resolution RGBA frame is fullW*fullH*4 bytes; the bug copies
	// two of them every frame. Allowing less than one frame's worth across the
	// entire loop guarantees no full-resolution copies were made for the
	// out-of-zone track, while leaving ample headroom for the small per-frame
	// tracker/zone bookkeeping.
	frameBytes := uint64(fullW * fullH * 4)
	if allocated >= frameBytes {
		t.Fatalf("out-of-zone track allocated %d bytes over %d frames (%.1f KB/frame); "+
			"expected < one full-res frame (%d bytes). Full-resolution snapshot copies are "+
			"not deferred for tracks that never emit an event.",
			allocated, iterations, float64(allocated)/float64(iterations)/1024, frameBytes)
	}
}

// TestRunTrackingPipeline_QualifyingTrackCapturesSnapshot proves the deferred
// capture still attaches a full-resolution clean and annotated snapshot to the
// start event of a track that does qualify - so deferring the copy fixes the
// runaway without dropping snapshots for real events.
func TestRunTrackingPipeline_QualifyingTrackCapturesSnapshot(t *testing.T) {
	const (
		detW  = 640
		detH  = 480
		fullW = 1920
		fullH = 1080
	)

	events := make(chan Event, 8)
	cam := NewTestCamera("snapcam")
	cam.tracker = detect.NewTracker(30, 3)
	cam.eventSnapDir = t.TempDir()
	cam.events = events
	// No zones configured: any confirmed track qualifies for an event.
	cam.snapConsumer = media.NewTestSnapshotConsumer(image.NewRGBA(image.Rect(0, 0, fullW, fullH)))

	buf := make([]byte, detW*detH*3)
	dets := []detect.Detection{{
		Label: "person",
		Score: 0.9,
		Box:   [4]int{100, 100, 300, 400},
	}}

	// Feed past minHits so the track confirms and emits its start event.
	for range 4 {
		cam.runTrackingPipeline(dets, rgbFrame(buf, detW, detH))
	}

	select {
	case ev := <-events:
		if ev.SnapshotImage == nil {
			t.Fatal("qualifying event has no clean snapshot image")
		}
		if ev.AnnotatedImage == nil {
			t.Fatal("qualifying event has no annotated snapshot image")
		}
		if b := ev.SnapshotImage.Bounds(); b.Dx() != fullW || b.Dy() != fullH {
			t.Fatalf("snapshot resolution = %dx%d, want full-res %dx%d", b.Dx(), b.Dy(), fullW, fullH)
		}
	default:
		t.Fatal("expected a start event with a snapshot for the confirmed track, got none")
	}
}
