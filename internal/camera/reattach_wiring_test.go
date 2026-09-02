package camera

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/rtsp"
)

// h264Track is the SDP a camera's video track arrives as. The parameter sets
// are a real 4-byte-aligned SPS/PPS pair so NewDetectConsumer builds a working
// decoder rather than reporting itself unavailable.
func silenceLogs(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

func h264Track() *rtsp.TrackInfo {
	return &rtsp.TrackInfo{
		Codec:       "H264",
		ClockRate:   90000,
		IsVideo:     true,
		PayloadType: 96,
		SPS:         []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2},
		PPS:         []byte{0x68, 0xce, 0x38, 0x80},
	}
}

// waitFor polls until cond holds, so a test does not depend on how long a
// goroutine takes to notice a tick.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The seam is proven in reattach_test.go; this covers the wiring, which is what
// actually decides whether a camera recovers. Detection is the camera's whole
// purpose, and its consumer is attached once at startup and then only read
// from, so a source-side detach is silent: frames stop and nothing asks why.
//
// The detach is performed with RemoveConsumer rather than by provoking a panic
// inside the H.264 decoder, because that is precisely the state the panic path
// leaves the source in - reattach_test.go drives a real panic through the real
// fan-out to establish that.
func TestDetectConsumerIsRebuiltAfterTheSourceDetachesIt(t *testing.T) {
	silenceLogs(t)

	const url = "rtsp://camera.invalid:554/stream"
	hub := rtsp.NewHub(context.Background())
	t.Cleanup(hub.Close)

	source := rtsp.NewSource(url)
	source.SetVideoTrack(h264Track())
	hub.SetSourceForTest(url, source)

	cam := &Camera{
		config: config.CameraConfig{
			Name:   "cam",
			URL:    url,
			Detect: config.DetectStreamConfig{Width: 640, Height: 480, FPS: 5},
		},
		hub:              hub,
		confirmedTracks:  make(map[int]string),
		trackNames:       make(map[int]string),
		presenceTracker:  NewPresenceTracker(),
		reattachInterval: 5 * time.Millisecond,
		mu:               sync.RWMutex{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		cam.readFrames(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	waitFor(t, "the detect consumer to attach", func() bool {
		return source.ConsumerCount() == 1
	})
	cam.mu.RLock()
	first := cam.detectConsumer
	cam.mu.RUnlock()
	if first == nil {
		t.Fatal("the camera did not record its detect consumer")
	}

	// What a panic inside the decoder leaves behind.
	source.RemoveConsumer(first)
	if source.ConsumerCount() != 0 {
		t.Fatal("the consumer was not detached, so there is nothing to recover from")
	}

	waitFor(t, "the detect consumer to be reattached", func() bool {
		return source.ConsumerCount() == 1
	})

	cam.mu.RLock()
	second := cam.detectConsumer
	cam.mu.RUnlock()
	if second == first {
		t.Error("the consumer whose decoder panicked was re-registered instead of rebuilt")
	}
	if !source.HasConsumer(second) {
		t.Error("the camera's recorded consumer is not the one attached to the source")
	}
}

// The accepting bound for the wiring. A healthy camera must keep the consumer
// it built: rebuilding one drops the decoder's reference frames, so a tick that
// replaced a working consumer would blank detection every few milliseconds here
// and every few seconds in production.
func TestDetectConsumerSurvivesTicksWhileAttached(t *testing.T) {
	silenceLogs(t)

	const url = "rtsp://camera.invalid:554/stream"
	hub := rtsp.NewHub(context.Background())
	t.Cleanup(hub.Close)

	source := rtsp.NewSource(url)
	source.SetVideoTrack(h264Track())
	hub.SetSourceForTest(url, source)

	cam := &Camera{
		config: config.CameraConfig{
			Name:   "cam",
			URL:    url,
			Detect: config.DetectStreamConfig{Width: 640, Height: 480, FPS: 5},
		},
		hub:              hub,
		confirmedTracks:  make(map[int]string),
		trackNames:       make(map[int]string),
		presenceTracker:  NewPresenceTracker(),
		reattachInterval: 2 * time.Millisecond,
		mu:               sync.RWMutex{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		cam.readFrames(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	waitFor(t, "the detect consumer to attach", func() bool {
		return source.ConsumerCount() == 1
	})
	cam.mu.RLock()
	first := cam.detectConsumer
	cam.mu.RUnlock()

	// Many ticks at the interval above.
	time.Sleep(100 * time.Millisecond)

	cam.mu.RLock()
	still := cam.detectConsumer
	cam.mu.RUnlock()
	if still != first {
		t.Error("an attached detect consumer was rebuilt")
	}
	if n := source.ConsumerCount(); n != 1 {
		t.Errorf("source has %d consumers, want 1: a rebuild leaked a registration", n)
	}
}

// The snapshot consumer decodes the main stream, and its frames are what an
// event snapshot and the live overlay are cut from. It is attached on its own
// goroutine that used to do nothing but wait for shutdown, so a detach left the
// camera detecting normally while every event snapshot silently fell back to
// the detect-resolution frame.
func TestSnapshotConsumerIsRebuiltAfterTheSourceDetachesIt(t *testing.T) {
	silenceLogs(t)

	const subURL = "rtsp://camera.invalid:554/sub"
	const mainURL = "rtsp://camera.invalid:554/main"
	hub := rtsp.NewHub(context.Background())
	t.Cleanup(hub.Close)

	sub := rtsp.NewSource(subURL)
	sub.SetVideoTrack(h264Track())
	hub.SetSourceForTest(subURL, sub)

	main := rtsp.NewSource(mainURL)
	main.SetVideoTrack(h264Track())
	hub.SetSourceForTest(mainURL, main)

	cam := &Camera{
		config: config.CameraConfig{
			Name:      "cam",
			URL:       subURL,
			RecordURL: mainURL,
			Detect:    config.DetectStreamConfig{Width: 640, Height: 480, FPS: 5},
		},
		hub:              hub,
		confirmedTracks:  make(map[int]string),
		trackNames:       make(map[int]string),
		presenceTracker:  NewPresenceTracker(),
		reattachInterval: 5 * time.Millisecond,
		mu:               sync.RWMutex{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		cam.readFrames(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	waitFor(t, "the snapshot consumer to attach to the main stream", func() bool {
		return main.ConsumerCount() == 1
	})
	first := cam.snapshotConsumer()
	if first == nil {
		t.Fatal("the camera did not record its snapshot consumer")
	}

	main.RemoveConsumer(first)
	if main.ConsumerCount() != 0 {
		t.Fatal("the consumer was not detached, so there is nothing to recover from")
	}

	waitFor(t, "the snapshot consumer to be reattached", func() bool {
		return main.ConsumerCount() == 1
	})
	second := cam.snapshotConsumer()
	if second == first {
		t.Error("the detached snapshot consumer was re-registered instead of rebuilt")
	}
	if !main.HasConsumer(second) {
		t.Error("the camera's recorded snapshot consumer is not the one attached to the source")
	}
	// The detect consumer on the sub stream is a different registration and
	// must not have been disturbed.
	if n := sub.ConsumerCount(); n != 1 {
		t.Errorf("sub stream has %d consumers, want 1: the rebuild touched the wrong source", n)
	}
}
