package event

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/storage"
)

// newLoopProcessor builds a processor whose handlers the test calls directly,
// in the order the Run loop would, so the loop's own state can be read.
func newLoopProcessor(t *testing.T, cfg *config.Config) *Processor {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	p, err := NewProcessor(Options{
		Config: cfg,
		DB:     db,
		Inputs: Inputs{
			Events:         make(chan camera.Event),
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
	t.Cleanup(func() {
		p.stopActiveTimers()
		p.background.Wait()
		_ = db.Close()
	})
	return p
}

func assertNothingWaits(t *testing.T, p *Processor) {
	t.Helper()
	if len(p.pendingEnds) != 0 {
		t.Errorf("an end is parked for an event that is never coming: %v", p.pendingEnds)
	}
	if len(p.settled) != 0 {
		t.Errorf("a settled event is still remembered after its end: %v", p.settled)
	}
}

// A track can outlive the event that MaxEventDuration ended. Its end arrives
// later and belongs to an event that is already closed, not to a lost one.
func TestEndAfterMaximumDurationIsNotParked(t *testing.T) {
	cfg := &config.Config{}
	cfg.Recording.MaxEventDuration = time.Hour
	p := newLoopProcessor(t, cfg)
	ctx := context.Background()

	p.acceptEvent(ctx, camera.Event{ID: "long", CameraName: "garage", Label: "bicycle", Timestamp: time.Now()})
	p.handleTimeout(ctx, "long")
	if _, open := p.active["long"]; open {
		t.Fatal("the event is still open after its timeout")
	}
	p.handleEnd(ctx, camera.EventEnd{EventID: "long", CameraName: "garage", EndTime: time.Now()})

	assertNothingWaits(t, p)
}

// A track whose event the cooldown suppressed still ends.
func TestEndOfACooldownSuppressedEventIsNotParked(t *testing.T) {
	cfg := &config.Config{}
	cfg.Recording.MaxEventDuration = time.Hour
	cfg.Events.CooldownSeconds = 60
	p := newLoopProcessor(t, cfg)
	ctx := context.Background()

	p.acceptEvent(ctx, camera.Event{ID: "first", CameraName: "front", Label: "person", Timestamp: time.Now()})
	p.handleEnd(ctx, camera.EventEnd{EventID: "first", CameraName: "front", EndTime: time.Now()})
	p.acceptEvent(ctx, camera.Event{ID: "suppressed", CameraName: "front", Label: "person", Timestamp: time.Now()})
	if _, open := p.active["suppressed"]; open {
		t.Fatal("the cooldown did not suppress the second event")
	}
	p.handleEnd(ctx, camera.EventEnd{EventID: "suppressed", CameraName: "front", EndTime: time.Now()})

	assertNothingWaits(t, p)
}

// The end can be serviced before the event it belongs to, also when that
// event is then suppressed.
func TestEndParkedBeforeASuppressedEventIsDropped(t *testing.T) {
	cfg := &config.Config{}
	cfg.Recording.MaxEventDuration = time.Hour
	cfg.Events.CooldownSeconds = 60
	p := newLoopProcessor(t, cfg)
	ctx := context.Background()

	p.acceptEvent(ctx, camera.Event{ID: "first", CameraName: "front", Label: "person", Timestamp: time.Now()})
	p.handleEnd(ctx, camera.EventEnd{EventID: "first", CameraName: "front", EndTime: time.Now()})
	p.handleEnd(ctx, camera.EventEnd{EventID: "suppressed", CameraName: "front", EndTime: time.Now()})
	p.acceptEvent(ctx, camera.Event{ID: "suppressed", CameraName: "front", Label: "person", Timestamp: time.Now()})

	assertNothingWaits(t, p)
}

// An end nothing accounts for is still parked, so that its event can adopt it
// and a lost one is reported.
func TestEndOfAnUnknownEventIsStillParked(t *testing.T) {
	cfg := &config.Config{}
	cfg.Recording.MaxEventDuration = time.Hour
	p := newLoopProcessor(t, cfg)

	p.handleEnd(context.Background(), camera.EventEnd{EventID: "early", CameraName: "front", EndTime: time.Now()})

	if _, parked := p.pendingEnds["early"]; !parked {
		t.Error("an end ahead of its event was not parked")
	}
}

func TestSettledEventsAreCapped(t *testing.T) {
	p := &Processor{settled: make(map[string]time.Time)}
	p.settled["oldest"] = time.Now().Add(-time.Hour)
	for i := range maxSettledEvents + 10 {
		p.settle("event-" + strconv.Itoa(i))
	}
	if got := len(p.settled); got > maxSettledEvents {
		t.Fatalf("remembered %d settled events, want at most %d", got, maxSettledEvents)
	}
	if _, kept := p.settled["oldest"]; kept {
		t.Error("the oldest settled event survived eviction")
	}
}
