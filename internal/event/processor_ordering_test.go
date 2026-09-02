package event_test

import (
	"context"
	"image"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/config"
	eventprocessor "github.com/rvben/vedetta/internal/event"
	"github.com/rvben/vedetta/internal/recording"
)

// blockingNotifier stalls EmitEventArtifacts until it is released, which is
// what a slow push endpoint does in production.
type blockingNotifier struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingNotifier() *blockingNotifier {
	return &blockingNotifier{entered: make(chan struct{}), release: make(chan struct{})}
}

func (n *blockingNotifier) Enqueue(camera.Event) {
	n.once.Do(func() { close(n.entered) })
	<-n.release
}

// waitForDrainedEnds blocks until the Run loop has taken every end off the
// channel. The loop is a single goroutine, so once it has consumed an end it
// has also finished handling it before it selects again.
func waitForDrainedEnds(t *testing.T, ends chan camera.EventEnd) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(ends) == 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("processor never consumed the event end")
}

// Events and ends travel on separate channels with no ordering between them,
// so a busy loop can service the end first. The end must still land on the
// event rather than being discarded, which would leave the event open until
// MaxEventDuration with a wrong end time and no log line.
func TestProcessorFinalizesEventWhoseEndArrivedFirst(t *testing.T) {
	cfg := &config.Config{}
	cfg.Recording.MaxEventDuration = time.Hour
	fixture := newRunningProcessor(t, cfg)

	started := time.Now().Add(-10 * time.Second).UTC().Truncate(time.Millisecond)
	ended := started.Add(4 * time.Second)

	fixture.ends <- camera.EventEnd{EventID: "out-of-order", CameraName: "front", EndTime: ended}
	waitForDrainedEnds(t, fixture.ends)

	fixture.events <- camera.Event{
		ID: "out-of-order", CameraName: "front", Label: "person", Timestamp: started,
	}

	waitForStoredEvent(t, fixture.db, "out-of-order", func(got *camera.Event) bool {
		return got.EndTime.Equal(ended)
	})
}

// finalizeEvent used to wait for the snapshot and notification goroutine on the
// Run loop itself, so one slow push endpoint stalled every other camera's
// begins and ends for up to emitWaitTimeout.
func TestProcessorSlowArtifactEmitDoesNotStallTheLoop(t *testing.T) {
	cfg := &config.Config{}
	cfg.Recording.MaxEventDuration = time.Hour
	notifier := newBlockingNotifier()
	defer close(notifier.release)

	fixture := newRunningProcessor(t, cfg, func(options *eventprocessor.Options) {
		options.Notifier = notifier
	})

	started := time.Now().Add(-10 * time.Second).UTC().Truncate(time.Millisecond)
	fixture.events <- camera.Event{
		ID: "slow-emit", CameraName: "front", Label: "person",
		Category: camera.CategoryAlert, Timestamp: started,
	}
	waitForStoredEvent(t, fixture.db, "slow-emit", func(*camera.Event) bool { return true })

	select {
	case <-notifier.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("notifier was never called, the test cannot stall the emit goroutine")
	}

	// Ending this event makes the loop wait on the stalled emit. A second
	// camera's event must not be held up behind it.
	fixture.ends <- camera.EventEnd{
		EventID: "slow-emit", CameraName: "front", EndTime: started.Add(2 * time.Second),
	}
	fixture.events <- camera.Event{
		ID: "behind-slow-emit", CameraName: "garage", Label: "car",
		Category: camera.CategoryDetection, Timestamp: time.Now(),
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := fixture.db.GetEventByID("behind-slow-emit")
		if err != nil {
			t.Fatalf("GetEventByID: %v", err)
		}
		if got != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("a second camera's event was blocked behind a stalled artifact emit")
}

// countingRecorder records whether a temporary recording was ever started.
type countingRecorder struct {
	mu      sync.Mutex
	started []string
}

func (r *countingRecorder) StartTemporaryRecording(_ context.Context, cameraName, _ string) {
	r.mu.Lock()
	r.started = append(r.started, cameraName)
	r.mu.Unlock()
}

func (r *countingRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.started)
}

func (*countingRecorder) CameraURL(string) string { return "rtsp://192.0.2.10:554/stream" }

func (*countingRecorder) SaveEventSnapshot(camera.Event, *image.RGBA, string) (string, error) {
	return "", nil
}

func (*countingRecorder) SaveClip(context.Context, camera.Event) (recording.ClipStats, error) {
	return recording.ClipStats{}, nil
}

// A database that refuses the insert means the event does not exist. Nothing
// downstream may then start a recorder, arm a timer or extract a clip for it.
func TestProcessorDoesNotActOnAnEventItFailedToPersist(t *testing.T) {
	cfg := &config.Config{}
	cfg.Recording.MaxEventDuration = time.Hour
	cfg.Recording.Continuous = false

	db := newEventTestDB(t)
	// Closing the database makes every write fail the way a full or locked
	// disk does, without needing to fill one.
	if err := db.Close(); err != nil {
		t.Fatalf("closing test database: %v", err)
	}

	recorder := &countingRecorder{}
	events := make(chan camera.Event, 1)
	processor, err := eventprocessor.NewProcessor(eventprocessor.Options{
		Config:   cfg,
		DB:       db,
		Recorder: recorder,
		Inputs: eventprocessor.Inputs{
			Events:         events,
			EventEnds:      make(chan camera.EventEnd, 1),
			PresenceEvents: make(chan camera.PresenceEvent),
			FaceEvents:     make(chan camera.FaceEvent),
			MotionActivity: make(chan camera.MotionActivity),
			Detections:     make(chan camera.DetectionFrame),
		},
		Tracer: otel.Tracer("test"),
	})
	if err != nil {
		t.Fatalf("NewProcessor: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go processor.Run(ctx)

	events <- camera.Event{
		ID: "never-stored", CameraName: "front", Label: "person", Timestamp: time.Now(),
	}

	// Give the loop time to have done the wrong thing if it were going to.
	time.Sleep(250 * time.Millisecond)
	if got := recorder.count(); got != 0 {
		t.Fatalf("started %d temporary recordings for an event that was never persisted, want 0", got)
	}
}

// The processor detaches goroutines that use the recorder, the MQTT client and
// the object embedder. Shutdown closes all of those, so Run must not report
// itself finished while any of that work is still in flight.
func TestProcessorRunWaitsForDetachedWork(t *testing.T) {
	cfg := &config.Config{}
	cfg.Recording.MaxEventDuration = time.Hour
	notifier := newBlockingNotifier()

	fixture := newRunningProcessor(t, cfg, func(options *eventprocessor.Options) {
		options.Notifier = notifier
	})

	fixture.events <- camera.Event{
		ID: "in-flight", CameraName: "front", Label: "person",
		Category: camera.CategoryAlert, Timestamp: time.Now().UTC(),
	}

	select {
	case <-notifier.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("artifact emit never started")
	}

	fixture.cancel()

	select {
	case <-fixture.done:
		close(notifier.release)
		t.Fatal("Run returned while a detached goroutine was still running, so shutdown would close the subsystems it uses")
	case <-time.After(250 * time.Millisecond):
	}

	close(notifier.release)

	select {
	case <-fixture.done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the detached goroutine finished")
	}
}
