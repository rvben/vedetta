package event_test

import (
	"context"
	"image"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/detect"
	eventprocessor "github.com/rvben/vedetta/internal/event"
	"github.com/rvben/vedetta/internal/storage"
)

type blockingPublisher struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingPublisher() *blockingPublisher {
	return &blockingPublisher{started: make(chan struct{}), release: make(chan struct{})}
}

func (p *blockingPublisher) block() {
	p.once.Do(func() { close(p.started) })
	<-p.release
}

func (p *blockingPublisher) PublishEvent(camera.Event, []string) error {
	p.block()
	return nil
}

func (p *blockingPublisher) PublishSnapshot(string, string, []byte) { p.block() }
func (p *blockingPublisher) PublishDoorbell(string, string, []byte) { p.block() }
func (p *blockingPublisher) PublishObjectCount(string, string, int) error {
	p.block()
	return nil
}
func (p *blockingPublisher) PublishPresence(camera.PresenceEvent, string) error {
	p.block()
	return nil
}
func (p *blockingPublisher) PublishObjectSighting(string, camera.Event) { p.block() }

type recordingDoorbellNotifier struct {
	doorbells chan camera.Event
}

func (*recordingDoorbellNotifier) Enqueue(camera.Event) {}
func (n *recordingDoorbellNotifier) EnqueueDoorbell(event camera.Event) bool {
	select {
	case n.doorbells <- event:
		return true
	default:
		return false
	}
}

func TestProcessorStopsWhenContextIsCancelled(t *testing.T) {
	db := newEventTestDB(t)

	inputs := eventprocessor.Inputs{
		Events:         make(chan camera.Event),
		EventEnds:      make(chan camera.EventEnd),
		PresenceEvents: make(chan camera.PresenceEvent),
		FaceEvents:     make(chan camera.FaceEvent),
		MotionActivity: make(chan camera.MotionActivity),
		Detections:     make(chan camera.DetectionFrame),
	}
	processor, err := eventprocessor.NewProcessor(eventprocessor.Options{
		Config: &config.Config{},
		DB:     db,
		Inputs: inputs,
		Tracer: otel.Tracer("test"),
	})
	if err != nil {
		t.Fatalf("NewProcessor: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		processor.Run(ctx)
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("processor did not stop after context cancellation")
	}
}

type recordingActivityNotifier struct {
	events     chan camera.Event
	activities chan storage.Activity
}

func (n *recordingActivityNotifier) Enqueue(event camera.Event) {
	n.events <- event
}

func (n *recordingActivityNotifier) EnqueueActivity(activity storage.Activity) bool {
	select {
	case n.activities <- activity:
		return true
	default:
		return false
	}
}

func TestProcessorFinalizesAndQueuesActivityOnStartup(t *testing.T) {
	db := newEventTestDB(t)
	event := camera.Event{
		ID: "old", CameraName: "front_door", Label: "person", Score: 0.9,
		Timestamp: time.Now().Add(-5 * time.Minute), Category: camera.CategoryAlert,
	}
	if err := db.SaveEvent(event); err != nil {
		t.Fatal(err)
	}
	inputs := eventprocessor.Inputs{
		Events: make(chan camera.Event), EventEnds: make(chan camera.EventEnd),
		PresenceEvents: make(chan camera.PresenceEvent), FaceEvents: make(chan camera.FaceEvent),
		MotionActivity: make(chan camera.MotionActivity), Detections: make(chan camera.DetectionFrame),
	}
	notifier := &recordingActivityNotifier{
		events: make(chan camera.Event, 1), activities: make(chan storage.Activity, 1),
	}
	processor, err := eventprocessor.NewProcessor(eventprocessor.Options{
		Config: &config.Config{}, DB: db, Inputs: inputs, Notifier: notifier, Tracer: otel.Tracer("test"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { processor.Run(ctx); close(done) }()

	select {
	case activity := <-notifier.activities:
		if activity.ID != "act_old" || activity.State != storage.ActivityStateFinalized {
			t.Fatalf("queued activity = %+v", activity)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for finalized activity")
	}
	select {
	case unexpected := <-notifier.events:
		t.Fatalf("queued raw event instead of activity: %+v", unexpected)
	default:
	}
	deadline := time.Now().Add(time.Second)
	for {
		pending, err := db.PendingActivityNotifications(10)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("activity notification was not durably marked")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}

func TestProcessorDoesNotReNotifyAnsweredDoorbellActivity(t *testing.T) {
	db := newEventTestDB(t)
	ring := camera.Event{
		ID: "answered-ring", CameraName: "front_door", Label: "doorbell",
		Kind: camera.EventKindDoorbell, Score: 1, Timestamp: time.Now().Add(-5 * time.Minute), Category: camera.CategoryAlert,
	}
	if err := db.SaveEvent(ring); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateEventAnswered(ring.ID, time.Now().Add(-4*time.Minute), "alice"); err != nil {
		t.Fatal(err)
	}
	inputs := eventprocessor.Inputs{
		Events: make(chan camera.Event), EventEnds: make(chan camera.EventEnd),
		PresenceEvents: make(chan camera.PresenceEvent), FaceEvents: make(chan camera.FaceEvent),
		MotionActivity: make(chan camera.MotionActivity), Detections: make(chan camera.DetectionFrame),
	}
	notifier := &recordingActivityNotifier{events: make(chan camera.Event, 1), activities: make(chan storage.Activity, 1)}
	processor, err := eventprocessor.NewProcessor(eventprocessor.Options{
		Config: &config.Config{}, DB: db, Inputs: inputs, Notifier: notifier, Tracer: otel.Tracer("test"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { processor.Run(ctx); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		activity, activityErr := db.GetActivityByID("act_answered-ring")
		if activityErr != nil {
			t.Fatal(activityErr)
		}
		pending, pendingErr := db.PendingActivityNotifications(10)
		if pendingErr != nil {
			t.Fatal(pendingErr)
		}
		if activity != nil && activity.State == storage.ActivityStateFinalized && len(pending) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("answered ring Activity remained pending")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case got := <-notifier.activities:
		t.Fatalf("answered ring produced delayed notification: %+v", got)
	default:
	}
	cancel()
	<-done
}

func TestProcessorPersistsSubmittedEvent(t *testing.T) {
	db := newEventTestDB(t)

	events := make(chan camera.Event, 1)
	cfg := &config.Config{}
	cfg.Recording.MaxEventDuration = time.Hour
	processor, err := eventprocessor.NewProcessor(eventprocessor.Options{
		Config: cfg,
		DB:     db,
		Inputs: eventprocessor.Inputs{
			Events:         events,
			EventEnds:      make(chan camera.EventEnd),
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

	want := camera.Event{
		ID:         "front-42",
		CameraName: "front",
		Label:      "person",
		Score:      0.93,
		TrackID:    42,
		Timestamp:  time.Now(),
	}
	events <- want

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		got, err := db.GetEventByID(want.ID)
		if err != nil {
			t.Fatalf("GetEventByID: %v", err)
		}
		if got != nil {
			if got.CameraName != want.CameraName || got.Label != want.Label || got.Score != want.Score {
				t.Fatalf("persisted event = %+v, want camera=%q label=%q score=%v", got, want.CameraName, want.Label, want.Score)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("submitted event was not persisted")
}

func TestProcessorMQTTStallDoesNotDelayEventPersistenceOrFinalization(t *testing.T) {
	cfg := &config.Config{}
	cfg.Recording.MaxEventDuration = time.Hour
	publisher := newBlockingPublisher()
	notifier := &recordingDoorbellNotifier{doorbells: make(chan camera.Event, 1)}
	defer close(publisher.release)
	fixture := newRunningProcessor(t, cfg, func(options *eventprocessor.Options) {
		options.Publisher = func() eventprocessor.Publisher { return publisher }
		options.Notifier = notifier
	})

	started := time.Now().Add(-time.Second).UTC().Truncate(time.Millisecond)
	fixture.events <- camera.Event{
		ID: "mqtt-blocker", CameraName: "front", Label: "person", Timestamp: started,
	}
	waitForStoredEvent(t, fixture.db, "mqtt-blocker", func(got *camera.Event) bool { return true })
	select {
	case <-publisher.started:
	case <-time.After(time.Second):
		t.Fatal("publisher did not enter blocking call")
	}

	fixture.events <- camera.Event{
		ID: "doorbell-behind-mqtt", CameraName: "front", Label: "doorbell",
		Kind: camera.EventKindDoorbell, Category: camera.CategoryAlert, Timestamp: time.Now(),
	}
	waitForStoredEvent(t, fixture.db, "doorbell-behind-mqtt", func(got *camera.Event) bool { return true })
	select {
	case got := <-notifier.doorbells:
		if got.ID != "doorbell-behind-mqtt" {
			t.Fatalf("notified doorbell = %q", got.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("doorbell notification was delayed by blocked MQTT delivery")
	}

	ended := started.Add(2 * time.Second)
	fixture.ends <- camera.EventEnd{EventID: "mqtt-blocker", CameraName: "front", EndTime: ended}
	waitForStoredEvent(t, fixture.db, "mqtt-blocker", func(got *camera.Event) bool {
		return got.EndTime.Equal(ended)
	})
}

func TestProcessorPersistsEventEnd(t *testing.T) {
	db := newEventTestDB(t)

	events := make(chan camera.Event, 1)
	ends := make(chan camera.EventEnd, 1)
	cfg := &config.Config{}
	cfg.Recording.MaxEventDuration = time.Hour
	processor, err := eventprocessor.NewProcessor(eventprocessor.Options{
		Config: cfg,
		DB:     db,
		Inputs: eventprocessor.Inputs{
			Events:         events,
			EventEnds:      ends,
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

	started := time.Now().Add(-time.Minute).UTC().Truncate(time.Millisecond)
	ended := started.Add(30 * time.Second)
	events <- camera.Event{ID: "front-end", CameraName: "front", Label: "person", Timestamp: started}
	waitForStoredEvent(t, db, "front-end", func(got *camera.Event) bool { return got.EndTime.IsZero() })
	ends <- camera.EventEnd{EventID: "front-end", CameraName: "front", EndTime: ended}

	waitForStoredEvent(t, db, "front-end", func(got *camera.Event) bool {
		return got.EndTime.Equal(ended)
	})
}

func TestProcessorEndsDoorbellEventAtClipWindow(t *testing.T) {
	cfg := &config.Config{}
	cfg.Recording.MaxEventDuration = time.Hour
	cfg.Doorbell.ClipSeconds = 1
	fixture := newRunningProcessor(t, cfg)

	started := time.Now().Add(-2 * time.Second).UTC().Truncate(time.Millisecond)
	fixture.events <- camera.Event{
		ID: "doorbell-press", CameraName: "front", Label: "doorbell",
		Kind: camera.EventKindDoorbell, Timestamp: started,
	}
	wantEnd := started.Add(time.Second)
	waitForStoredEvent(t, fixture.db, "doorbell-press", func(got *camera.Event) bool {
		return got.EndTime.Equal(wantEnd)
	})
}

func TestProcessorCancelsPendingDoorbellEndOnShutdown(t *testing.T) {
	cfg := &config.Config{}
	cfg.Recording.MaxEventDuration = time.Hour
	cfg.Doorbell.ClipSeconds = 1
	fixture := newRunningProcessor(t, cfg)

	fixture.events <- camera.Event{
		ID: "doorbell-shutdown", CameraName: "front", Label: "doorbell",
		Kind: camera.EventKindDoorbell, Timestamp: time.Now().Add(-800 * time.Millisecond),
	}
	waitForStoredEvent(t, fixture.db, "doorbell-shutdown", func(got *camera.Event) bool { return true })
	fixture.cancel()
	select {
	case <-fixture.done:
	case <-time.After(time.Second):
		t.Fatal("processor did not stop")
	}

	select {
	case end := <-fixture.ends:
		t.Fatalf("received synthetic end after processor shutdown: %+v", end)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestProcessorEndsEventAtMaximumDuration(t *testing.T) {
	cfg := &config.Config{}
	cfg.Recording.MaxEventDuration = 30 * time.Millisecond
	fixture := newRunningProcessor(t, cfg)

	started := time.Now().UTC().Truncate(time.Millisecond)
	fixture.events <- camera.Event{
		ID: "front-timeout", CameraName: "front", Label: "person", Timestamp: started,
	}
	wantEnd := started.Add(cfg.Recording.MaxEventDuration)
	waitForStoredEvent(t, fixture.db, "front-timeout", func(got *camera.Event) bool {
		return got.EndTime.Equal(wantEnd)
	})
}

func TestProcessorSuppressesMatchingEventDuringCooldown(t *testing.T) {
	cfg := &config.Config{}
	cfg.Recording.MaxEventDuration = time.Hour
	cfg.Events.CooldownSeconds = 60
	fixture := newRunningProcessor(t, cfg)

	started := time.Now().Add(-time.Second).UTC().Truncate(time.Millisecond)
	fixture.events <- camera.Event{
		ID: "first", CameraName: "front", Label: "person", ZoneName: "driveway", Timestamp: started,
	}
	waitForStoredEvent(t, fixture.db, "first", func(got *camera.Event) bool { return true })
	fixture.ends <- camera.EventEnd{EventID: "first", CameraName: "front", EndTime: time.Now()}
	waitForStoredEvent(t, fixture.db, "first", func(got *camera.Event) bool { return !got.EndTime.IsZero() })

	fixture.events <- camera.Event{
		ID: "suppressed", CameraName: "front", Label: "person", ZoneName: "driveway", Timestamp: time.Now(),
	}
	// A different zone acts as a barrier proving the processor consumed the
	// suppressed event before this one.
	fixture.events <- camera.Event{
		ID: "barrier", CameraName: "front", Label: "person", ZoneName: "porch", Timestamp: time.Now(),
	}
	waitForStoredEvent(t, fixture.db, "barrier", func(got *camera.Event) bool { return true })

	suppressed, err := fixture.db.GetEventByID("suppressed")
	if err != nil {
		t.Fatalf("GetEventByID(suppressed): %v", err)
	}
	if suppressed != nil {
		t.Fatalf("cooldown event was persisted: %+v", suppressed)
	}
}

func TestProcessorPersistsMotionActivity(t *testing.T) {
	cfg := &config.Config{}
	fixture := newRunningProcessor(t, cfg)
	bucket := time.Now().UTC().Truncate(time.Minute)
	fixture.motion <- camera.MotionActivity{CameraName: "front", Bucket: bucket, Score: 0.73}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		activity, err := fixture.db.GetMotionActivityInRange("front", bucket, bucket.Add(time.Minute))
		if err != nil {
			t.Fatalf("GetMotionActivityInRange: %v", err)
		}
		if len(activity) == 1 {
			if activity[0].Score != 0.73 {
				t.Fatalf("motion score = %v, want 0.73", activity[0].Score)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("motion activity was not persisted")
}

func TestProcessorPersistsZonePresence(t *testing.T) {
	cfg := &config.Config{}
	fixture := newRunningProcessor(t, cfg)
	if err := fixture.db.SaveZone(camera.Zone{
		Camera: "front", Name: "driveway", Labels: []string{"person"}, Enabled: true,
	}); err != nil {
		t.Fatalf("SaveZone: %v", err)
	}
	zone, err := fixture.db.GetZone("front", "driveway")
	if err != nil || zone == nil {
		t.Fatalf("GetZone: zone=%v err=%v", zone, err)
	}
	fixture.presence <- camera.PresenceEvent{
		ZoneID: zone.ID, ZoneName: zone.Name, Label: "person", Type: "zone_enter", Time: time.Now(),
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		states, err := fixture.db.GetZonePresence(zone.ID)
		if err != nil {
			t.Fatalf("GetZonePresence: %v", err)
		}
		if len(states) == 1 && states[0].Present {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("zone presence was not persisted")
}

func TestProcessorForwardsDetectionFrames(t *testing.T) {
	server := &recordingRuntimeServer{detections: make(chan camera.DetectionFrame, 1)}
	fixture := newRunningProcessor(t, &config.Config{}, func(options *eventprocessor.Options) {
		options.Server = server
	})
	want := camera.DetectionFrame{
		Camera: "front", Timestamp: time.Now(),
		Boxes: []camera.DetectionBox{{Label: "person", Score: 0.9, TrackID: 7}},
	}
	fixture.detections <- want

	select {
	case got := <-server.detections:
		if got.Camera != want.Camera || len(got.Boxes) != 1 || got.Boxes[0].TrackID != 7 {
			t.Fatalf("detection frame = %+v, want %+v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("detection frame was not forwarded")
	}
}

func TestProcessorEnrichesEventWhenFaceMatchesKnownPerson(t *testing.T) {
	cfg := &config.Config{}
	cfg.Recording.MaxEventDuration = time.Hour
	fixture := newRunningProcessor(t, cfg, func(options *eventprocessor.Options) {
		options.FaceRecognizer = thresholdRecognizer(0.5)
	})
	if _, err := fixture.db.SavePerson("Alice", false, detect.Float32ToBytes([]float32{1, 0})); err != nil {
		t.Fatalf("SavePerson: %v", err)
	}

	fixture.events <- camera.Event{
		ID: "face-event", CameraName: "front", Label: "person", Timestamp: time.Now(),
	}
	waitForStoredEvent(t, fixture.db, "face-event", func(got *camera.Event) bool { return true })
	fixture.faces <- camera.FaceEvent{
		Camera: "front", EventID: "face-event",
		Results: []detect.FaceResult{{Embedding: []float32{1, 0}, Confidence: 0.98}},
	}

	waitForStoredEvent(t, fixture.db, "face-event", func(got *camera.Event) bool {
		return got.SubLabel == "Alice"
	})
}

func TestProcessorEnrichesEventWhenObjectMatchesKnownObject(t *testing.T) {
	cfg := &config.Config{}
	cfg.Recording.MaxEventDuration = time.Hour
	cfg.Detect.ObjectMatchThreshold = 0.5
	fixture := newRunningProcessor(t, cfg, func(options *eventprocessor.Options) {
		options.ObjectEmbedder = fixedObjectEmbedder{embedding: []float32{1, 0}}
	})
	if _, err := fixture.db.SaveKnownObject(storage.KnownObject{
		Name: "Ruben's car", Label: "car", Centroid: detect.Float32ToBytes([]float32{1, 0}),
	}); err != nil {
		t.Fatalf("SaveKnownObject: %v", err)
	}

	fixture.events <- camera.Event{
		ID: "object-event", CameraName: "driveway", Label: "car", Timestamp: time.Now(),
		SnapshotImage: image.NewRGBA(image.Rect(0, 0, 2, 2)), Box: [4]int{0, 0, 2, 2},
	}

	waitForStoredEvent(t, fixture.db, "object-event", func(got *camera.Event) bool {
		return got.ObjectName == "Ruben's car" && got.SubLabel == "Ruben's car"
	})
}

type fixedObjectEmbedder struct {
	embedding []float32
}

func (e fixedObjectEmbedder) Embed(*image.RGBA, [4]int) ([]float32, error) {
	return e.embedding, nil
}

type thresholdRecognizer float64

func (r thresholdRecognizer) MatchThreshold() float64 { return float64(r) }

type recordingRuntimeServer struct {
	detections chan camera.DetectionFrame
}

func (*recordingRuntimeServer) RecordDoorbellPress(string)                  {}
func (*recordingRuntimeServer) BroadcastDoorbellSSE(string, string, string) {}
func (*recordingRuntimeServer) BroadcastDoorbellPersonSSE(string, string, string) {
}
func (*recordingRuntimeServer) BroadcastActivitySSE(string, storage.Activity) {}
func (s *recordingRuntimeServer) PublishDetection(frame camera.DetectionFrame) {
	s.detections <- frame
}

type runningProcessor struct {
	db         *storage.DB
	events     chan camera.Event
	ends       chan camera.EventEnd
	motion     chan camera.MotionActivity
	presence   chan camera.PresenceEvent
	detections chan camera.DetectionFrame
	faces      chan camera.FaceEvent
	cancel     context.CancelFunc
	done       <-chan struct{}
}

func newRunningProcessor(t *testing.T, cfg *config.Config, configure ...func(*eventprocessor.Options)) runningProcessor {
	t.Helper()
	db := newEventTestDB(t)
	events := make(chan camera.Event, 8)
	ends := make(chan camera.EventEnd, 8)
	motion := make(chan camera.MotionActivity, 8)
	presence := make(chan camera.PresenceEvent, 8)
	detections := make(chan camera.DetectionFrame, 8)
	faces := make(chan camera.FaceEvent, 8)
	options := eventprocessor.Options{
		Config: cfg,
		DB:     db,
		Inputs: eventprocessor.Inputs{
			Events:         events,
			EventEnds:      ends,
			PresenceEvents: presence,
			FaceEvents:     faces,
			MotionActivity: motion,
			Detections:     detections,
		},
		Tracer: otel.Tracer("test"),
	}
	for _, apply := range configure {
		apply(&options)
	}
	processor, err := eventprocessor.NewProcessor(options)
	if err != nil {
		t.Fatalf("NewProcessor: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		processor.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("processor did not stop during cleanup")
		}
	})
	return runningProcessor{
		db: db, events: events, ends: ends, motion: motion, presence: presence,
		detections: detections, faces: faces, cancel: cancel, done: done,
	}
}

func newEventTestDB(t *testing.T) *storage.DB {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func waitForStoredEvent(t *testing.T, db *storage.DB, eventID string, ready func(*camera.Event) bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		got, err := db.GetEventByID(eventID)
		if err != nil {
			t.Fatalf("GetEventByID: %v", err)
		}
		if got != nil && ready(got) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("event %q did not reach expected stored state", eventID)
}
