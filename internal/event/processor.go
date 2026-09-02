// Package event owns Vedetta's event lifecycle orchestration.
package event

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/detect"
	"github.com/rvben/vedetta/internal/storage"
)

const (
	emitWaitTimeout       = 5 * time.Second
	activitySweepInterval = 5 * time.Second

	// pendingEndTTL bounds how long an event end that arrived before its event
	// is held. Beyond this the begin is never coming and holding the end only
	// grows the map.
	pendingEndTTL = 2 * time.Minute

	// maxPendingEnds caps the parked-end map so a camera emitting ends for
	// events that never begin cannot grow it without limit.
	maxPendingEnds = 1024
)

// Inputs contains the event streams consumed by a Processor. EventEnds is
// bidirectional because the processor also schedules synthetic ends for
// point-in-time doorbell events.
type Inputs struct {
	Events         <-chan camera.Event
	EventEnds      chan camera.EventEnd
	PresenceEvents <-chan camera.PresenceEvent
	FaceEvents     <-chan camera.FaceEvent
	MotionActivity <-chan camera.MotionActivity
	Detections     <-chan camera.DetectionFrame
}

// Options contains the required runtime state for an event Processor.
type Options struct {
	Config         *config.Config
	DB             *storage.DB
	Recorder       Recorder
	Publisher      func() Publisher
	Notifier       Enqueuer
	Server         RuntimeServer
	Cameras        CameraLookup
	ObjectEmbedder ObjectEmbedder
	FaceRecognizer FaceRecognizer
	Inputs         Inputs
	Tracer         trace.Tracer
}

// Processor coordinates event lifecycle state on one goroutine.
type Processor struct {
	options        Options
	active         map[string]*activeEvent
	timeout        chan string
	mqttDispatcher *mqttPublishDispatcher

	objectCounts map[string]map[string]int
	// objectCountGate orders the publishes of objectCounts. The tally itself is
	// always right; only the publishes can arrive out of order.
	objectCountGate *objectCountGate
	cooldowns       map[string]time.Time

	// pendingEnds holds ends whose event has not been accepted yet. Events and
	// ends arrive on separate channels with no ordering between them, so under
	// load the end can be serviced first.
	pendingEnds map[string]pendingEnd

	// background counts the goroutines the processor detaches. Those goroutines
	// use the recorder, the MQTT client, the object embedder and the hub, so
	// Run waits for them before returning and the caller closes those
	// subsystems only after Run has returned.
	background sync.WaitGroup
}

// pendingEnd is an event end parked until its event arrives.
type pendingEnd struct {
	endTime  time.Time
	parkedAt time.Time
}

type activeEvent struct {
	event       camera.Event
	timer       *time.Timer
	endTimer    *time.Timer
	tempCancel  context.CancelFunc
	rootSpanCtx trace.SpanContext
	emitDone    chan struct{}
}

// NewProcessor validates and constructs an event Processor.
func NewProcessor(options Options) (*Processor, error) {
	if options.Config == nil {
		return nil, fmt.Errorf("event processor: config is required")
	}
	if options.DB == nil {
		return nil, fmt.Errorf("event processor: database is required")
	}
	if options.Tracer == nil {
		return nil, fmt.Errorf("event processor: tracer is required")
	}
	if options.Inputs.Events == nil || options.Inputs.EventEnds == nil ||
		options.Inputs.PresenceEvents == nil || options.Inputs.FaceEvents == nil ||
		options.Inputs.MotionActivity == nil || options.Inputs.Detections == nil {
		return nil, fmt.Errorf("event processor: all input channels are required")
	}
	processor := &Processor{
		options:         options,
		active:          make(map[string]*activeEvent),
		timeout:         make(chan string, 100),
		objectCounts:    make(map[string]map[string]int),
		objectCountGate: newObjectCountGate(),
		cooldowns:       make(map[string]time.Time),
		pendingEnds:     make(map[string]pendingEnd),
	}
	if options.Publisher != nil {
		processor.mqttDispatcher = newMQTTPublishDispatcher(options.Publisher, options.Tracer)
	}
	return processor, nil
}

// Run processes events until ctx is cancelled. It returns once the loop has
// stopped and every goroutine the processor detached has finished, so a caller
// can wait for it before closing the subsystems those goroutines use.
func (p *Processor) Run(ctx context.Context) {
	defer p.background.Wait()
	if p.mqttDispatcher != nil {
		p.goBackground(func() { p.mqttDispatcher.run(ctx) })
	}
	activityTicker := time.NewTicker(activitySweepInterval)
	defer activityTicker.Stop()
	p.finalizeActivities(time.Now())
	for {
		select {
		case <-ctx.Done():
			p.stopActiveTimers()
			return
		case now := <-activityTicker.C:
			p.finalizeActivities(now)
			p.expirePendingEnds(now)
		case submitted, ok := <-p.options.Inputs.Events:
			if !ok {
				p.stopActiveTimers()
				return
			}
			p.acceptEvent(ctx, submitted)
		case end, ok := <-p.options.Inputs.EventEnds:
			if !ok {
				p.stopActiveTimers()
				return
			}
			if active, exists := p.active[end.EventID]; exists {
				p.finalizeEvent(ctx, active, end.EndTime)
				delete(p.active, end.EventID)
			} else {
				p.parkEnd(end)
			}
		case eventID := <-p.timeout:
			if active, exists := p.active[eventID]; exists {
				endTime := active.event.Timestamp.Add(p.options.Config.Recording.MaxEventDuration)
				p.finalizeEvent(ctx, active, endTime)
				delete(p.active, eventID)
			}
		case presence, ok := <-p.options.Inputs.PresenceEvents:
			if !ok {
				p.stopActiveTimers()
				return
			}
			p.handlePresence(ctx, presence)
		case face, ok := <-p.options.Inputs.FaceEvents:
			if !ok {
				p.stopActiveTimers()
				return
			}
			p.handleFaces(face)
		case activity, ok := <-p.options.Inputs.MotionActivity:
			if !ok {
				p.stopActiveTimers()
				return
			}
			if err := p.options.DB.SaveMotionActivity(activity.CameraName, activity.Bucket, activity.Score); err != nil {
				slog.Error("failed to save motion activity", "camera", activity.CameraName, "error", err)
			}
		case detection, ok := <-p.options.Inputs.Detections:
			if !ok {
				p.stopActiveTimers()
				return
			}
			if p.options.Server != nil {
				p.options.Server.PublishDetection(detection)
			}
		}
	}
}

func (p *Processor) acceptEvent(ctx context.Context, submitted camera.Event) {
	if submitted.Kind != camera.EventKindDoorbell {
		if until, exists := p.cooldowns[cooldownKey(submitted)]; exists &&
			time.Since(until) < time.Duration(p.options.Config.Events.CooldownSeconds)*time.Second {
			slog.Info("event suppressed by cooldown", "camera", submitted.CameraName,
				"label", submitted.Label, "zone", submitted.ZoneName)
			return
		}
	}
	slog.Info("event detected", "camera", submitted.CameraName, "label", submitted.Label,
		"score", fmt.Sprintf("%.2f", submitted.Score))

	eventCtx, rootSpan, saveErr := p.persistEvent(ctx, submitted)
	if saveErr != nil {
		// The event does not exist, so nothing downstream may act as if it
		// does: no object count, no recording, no timers, no clip. Doing that
		// work anyway writes files and starts recorders that nothing
		// references, which is exactly the wrong response to a database that
		// is already failing.
		rootSpan.End()
		slog.Error("event dropped, not persisted", "camera", submitted.CameraName,
			"label", submitted.Label, "event", submitted.ID, "error", saveErr)
		return
	}
	p.publishActivityForEvent(submitted.ID, "activity_updated")
	if submitted.Kind == camera.EventKindDoorbell && p.options.Server != nil {
		p.options.Server.RecordDoorbellPress(submitted.CameraName)
		p.options.Server.BroadcastDoorbellSSE(submitted.CameraName, submitted.ID, submitted.SubLabel)
	}

	publisher := p.publisher(eventCtx)
	if publisher != nil && submitted.Kind != camera.EventKindDoorbell {
		if p.objectCounts[submitted.CameraName] == nil {
			p.objectCounts[submitted.CameraName] = make(map[string]int)
		}
		p.objectCounts[submitted.CameraName][submitted.Label]++
		count := p.objectCounts[submitted.CameraName][submitted.Label]
		seq := p.objectCountGate.reserve()
		p.objectCountGate.publish(submitted.CameraName, submitted.Label, seq, func() {
			SpanPublish(eventCtx, p.options.Tracer, "mqtt.publish_object_count", func() error {
				return publisher.PublishObjectCount(submitted.CameraName, submitted.Label, count)
			})
		})
	}

	emitDone := make(chan struct{})
	eventCopy := submitted
	p.goBackground(func() {
		defer close(emitDone)
		EmitEventArtifacts(eventCtx, p.options.Tracer, p.options.Recorder, publisher,
			p.options.Notifier, p.options.Config.Events.SnapshotQuality, eventCopy)
	})

	if p.options.ObjectEmbedder != nil && submitted.SnapshotImage != nil {
		p.goBackground(func() { p.recognizeObject(eventCtx, submitted) })
	}

	rootSpanCtx := rootSpan.SpanContext()
	rootSpan.End()

	var tempCancel context.CancelFunc
	if !p.options.Config.Recording.Continuous && p.options.Recorder != nil {
		if url := p.options.Recorder.CameraURL(submitted.CameraName); url != "" {
			tempCtx, cancel := context.WithCancel(ctx)
			tempCancel = cancel
			p.options.Recorder.StartTemporaryRecording(tempCtx, submitted.CameraName, url)
		}
	}

	eventID := submitted.ID
	timer := time.AfterFunc(p.options.Config.Recording.MaxEventDuration, func() {
		select {
		case p.timeout <- eventID:
		default:
		}
	})
	active := &activeEvent{
		event: submitted, timer: timer, tempCancel: tempCancel,
		rootSpanCtx: rootSpanCtx, emitDone: emitDone,
	}
	p.active[eventID] = active

	// The end may already have been serviced ahead of this begin. Adopting it
	// here is what keeps the stored end time, the MQTT end publish and the clip
	// window truthful instead of waiting out MaxEventDuration.
	if parked, waiting := p.pendingEnds[eventID]; waiting {
		delete(p.pendingEnds, eventID)
		slog.Info("event end arrived before its event, finalizing now",
			"event", eventID, "camera", submitted.CameraName,
			"waited", time.Since(parked.parkedAt).Round(time.Millisecond))
		p.finalizeEvent(ctx, active, parked.endTime)
		delete(p.active, eventID)
		return
	}

	if submitted.Kind == camera.EventKindDoorbell {
		endTime := submitted.Timestamp.Add(DoorbellClipWindow(p.options.Config, submitted.CameraName))
		delay := time.Until(endTime)
		if delay < 0 {
			delay = 0
		}
		active.endTimer = time.AfterFunc(delay, func() {
			if ctx.Err() != nil {
				return
			}
			select {
			case <-ctx.Done():
			case p.options.Inputs.EventEnds <- camera.EventEnd{
				EventID: eventID, CameraName: submitted.CameraName, EndTime: endTime,
			}:
			default:
			}
		})
	}
}

func (p *Processor) persistEvent(ctx context.Context, submitted camera.Event) (context.Context, trace.Span, error) {
	eventCtx, eventSpan := p.options.Tracer.Start(ctx, "event", trace.WithAttributes(
		attribute.String("vedetta.camera", submitted.CameraName),
		attribute.String("vedetta.label", submitted.Label),
		attribute.Int("vedetta.track_id", submitted.TrackID),
		attribute.String("vedetta.event_id", submitted.ID),
		attribute.Float64("vedetta.score", float64(submitted.Score)),
	))
	if submitted.ZoneName != "" {
		eventSpan.SetAttributes(attribute.String("vedetta.zone", submitted.ZoneName))
	}

	_, dbSpan := p.options.Tracer.Start(eventCtx, "db.save_event")
	saveErr := p.options.DB.SaveEvent(submitted)
	if saveErr != nil {
		dbSpan.RecordError(saveErr)
		dbSpan.SetStatus(codes.Error, "save event")
		slog.Error("failed to save event", "error", saveErr)
	}
	dbSpan.End()
	return eventCtx, eventSpan, saveErr
}

// parkEnd holds an end whose event has not been accepted yet, so that
// acceptEvent can adopt it. Without this the end is discarded and the event
// stays open until MaxEventDuration, with a wrong stored end time, a wrong
// clip window and no log line to say so.
// goBackground runs fn on its own goroutine and counts it, so Run can wait for
// it during shutdown.
func (p *Processor) goBackground(fn func()) {
	p.background.Add(1)
	go func() {
		defer p.background.Done()
		fn()
	}()
}

func (p *Processor) parkEnd(end camera.EventEnd) {
	if len(p.pendingEnds) >= maxPendingEnds {
		var oldestID string
		var oldest time.Time
		for id, parked := range p.pendingEnds {
			if oldestID == "" || parked.parkedAt.Before(oldest) {
				oldestID, oldest = id, parked.parkedAt
			}
		}
		delete(p.pendingEnds, oldestID)
		slog.Warn("pending event ends at capacity, dropping oldest",
			"dropped_event", oldestID, "capacity", maxPendingEnds)
	}
	p.pendingEnds[end.EventID] = pendingEnd{endTime: end.EndTime, parkedAt: time.Now()}
	slog.Debug("event end arrived before its event, parking it",
		"event", end.EventID, "camera", end.CameraName)
}

// expirePendingEnds drops parked ends whose event never arrived. Each one is a
// genuinely lost end, so it is logged rather than discarded quietly.
func (p *Processor) expirePendingEnds(now time.Time) {
	for eventID, parked := range p.pendingEnds {
		if now.Sub(parked.parkedAt) < pendingEndTTL {
			continue
		}
		delete(p.pendingEnds, eventID)
		slog.Warn("event end discarded, its event never arrived",
			"event", eventID, "parked_for", now.Sub(parked.parkedAt).Round(time.Second))
	}
}

func (p *Processor) finalizeEvent(ctx context.Context, active *activeEvent, endTime time.Time) {
	active.timer.Stop()
	if active.endTimer != nil {
		active.endTimer.Stop()
	}
	event := active.event
	event.EndTime = endTime

	endCtx := trace.ContextWithSpanContext(ctx, active.rootSpanCtx)
	_, endSpan := p.options.Tracer.Start(endCtx, "event.end")
	if err := p.options.DB.UpdateEventEndTime(event.ID, endTime); err != nil {
		endSpan.RecordError(err)
		endSpan.SetStatus(codes.Error, "update end time")
		slog.Error("failed to update event end time", "event", event.ID, "error", err)
	} else {
		p.publishActivityForEvent(event.ID, "activity_updated")
	}

	// objectCounts belongs to the Run loop, so the decrement happens here and
	// only the resulting value crosses to the publishing goroutine.
	objectCount := -1
	var objectCountSeq uint64
	if event.Kind != camera.EventKindDoorbell {
		if counts, exists := p.objectCounts[event.CameraName]; exists {
			counts[event.Label]--
			if counts[event.Label] < 0 {
				counts[event.Label] = 0
			}
			objectCount = counts[event.Label]
			// Reserved here rather than at the publish: this is where the count
			// changes, and the sequence has to record that order, not the order
			// the background goroutines happen to finish waiting in.
			objectCountSeq = p.objectCountGate.reserve()
		}
	}

	// Waiting for the snapshot and notification goroutine used to happen on the
	// Run loop, where a slow push endpoint stalled every other camera's begins
	// and ends for up to emitWaitTimeout. The loop now does bookkeeping only.
	publisher := p.publisher(endCtx)
	emitDone := active.emitDone
	p.goBackground(func() {
		p.publishEventEnd(ctx, endCtx, endSpan, event, emitDone, publisher, objectCount, objectCountSeq)
	})

	slog.Info("event ended", "event", event.ID, "camera", event.CameraName,
		"label", event.Label, "duration", endTime.Sub(event.Timestamp).Round(time.Second))
	p.cooldowns[cooldownKey(event)] = endTime

	if active.tempCancel != nil {
		temporaryCancel := active.tempCancel
		p.goBackground(func() {
			timer := time.NewTimer(p.options.Config.Recording.PostCapture + 5*time.Second)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
			}
			temporaryCancel()
		})
	}

	if p.options.Recorder != nil {
		clipCtx := trace.ContextWithSpanContext(ctx, active.rootSpanCtx)
		p.goBackground(func() { p.extractClip(clipCtx, event) })
	}
}

// publishEventEnd waits for the event's artifacts and then publishes the end,
// off the Run loop. The snapshot must be out before the end publish so a
// subscriber that reacts to the end can already fetch it, which is why the wait
// stayed in front of the publish rather than being dropped.
func (p *Processor) publishEventEnd(ctx, endCtx context.Context, endSpan trace.Span,
	event camera.Event, emitDone <-chan struct{}, publisher Publisher, objectCount int, objectCountSeq uint64) {
	defer endSpan.End()

	WaitForEmit(ctx, emitDone, emitWaitTimeout)
	if publisher == nil {
		return
	}
	SpanPublish(endCtx, p.options.Tracer, "mqtt.publish_event_end", func() error {
		if err := publisher.PublishEvent(event, nil); err != nil {
			slog.Error("failed to publish event end", "event", event.ID, "error", err)
			return err
		}
		return nil
	})
	// The wait above is why this needs the gate: it can outlast the next change
	// to the same count, and the sensor is retained, so publishing a value the
	// Run loop has already moved past leaves it wrong until the next event.
	if objectCount >= 0 {
		p.objectCountGate.publish(event.CameraName, event.Label, objectCountSeq, func() {
			SpanPublish(endCtx, p.options.Tracer, "mqtt.publish_object_count", func() error {
				return publisher.PublishObjectCount(event.CameraName, event.Label, objectCount)
			})
		})
	}
}

func (p *Processor) publishActivityForEvent(eventID, eventType string) {
	if p.options.Server == nil {
		return
	}
	activity, err := p.options.DB.GetActivityByEventID(eventID)
	if err != nil {
		slog.Error("failed to load activity for live update", "event", eventID, "error", err)
		return
	}
	if activity != nil {
		p.options.Server.BroadcastActivitySSE(eventType, *activity)
	}
}

func (p *Processor) finalizeActivities(now time.Time) {
	activities, err := p.options.DB.FinalizeDueActivities(now, 100)
	if err != nil {
		slog.Error("failed to finalize activities", "error", err)
		return
	}
	if p.options.Server != nil {
		for _, activity := range activities {
			p.options.Server.BroadcastActivitySSE("activity_finalized", activity)
		}
	}
	p.enqueuePendingActivityNotifications(now)
}

func (p *Processor) enqueuePendingActivityNotifications(now time.Time) {
	enqueuer, ok := p.options.Notifier.(ActivityEnqueuer)
	if !ok {
		return
	}
	pending, err := p.options.DB.PendingActivityNotifications(100)
	if err != nil {
		slog.Error("failed to load pending activity notifications", "error", err)
		return
	}
	for _, activity := range pending {
		// The immediate ring push already opened the answer surface. Once a
		// household member acknowledges every ring in this Activity, a delayed
		// finalized notification would only re-alert them after the fact.
		if activity.HasDoorbell && !activity.MissedDoorbell {
			if _, err := p.options.DB.MarkActivityNotificationQueued(activity.ID, now); err != nil {
				slog.Error("failed to suppress answered doorbell notification", "activity", activity.ID, "error", err)
				return
			}
			continue
		}
		if !enqueuer.EnqueueActivity(activity) {
			return
		}
		if _, err := p.options.DB.MarkActivityNotificationQueued(activity.ID, now); err != nil {
			slog.Error("failed to mark activity notification queued", "activity", activity.ID, "error", err)
			return
		}
	}
}

func (p *Processor) stopActiveTimers() {
	for eventID, active := range p.active {
		active.timer.Stop()
		if active.endTimer != nil {
			active.endTimer.Stop()
		}
		if active.tempCancel != nil {
			active.tempCancel()
		}
		delete(p.active, eventID)
	}
}

func (p *Processor) publisher(ctx context.Context) Publisher {
	if p.mqttDispatcher == nil || p.options.Publisher() == nil {
		return nil
	}
	return &queuedPublisher{ctx: ctx, dispatcher: p.mqttDispatcher}
}

func (p *Processor) recognizeObject(ctx context.Context, submitted camera.Event) {
	_, span := p.options.Tracer.Start(ctx, "object.reid")
	defer span.End()
	matched := matchEventToKnownObjects(p.options.DB, p.options.ObjectEmbedder, submitted,
		p.options.Config.Detect.ObjectMatchThreshold)
	if len(matched) > 0 && p.options.Cameras != nil {
		if liveCamera := p.options.Cameras.GetCamera(submitted.CameraName); liveCamera != nil {
			liveCamera.SetTrackName(submitted.TrackID, matched[0])
		}
	}
	if publisher := p.publisher(ctx); publisher != nil {
		for _, name := range matched {
			publisher.PublishObjectSighting(name, submitted)
		}
	}
}

func (p *Processor) extractClip(ctx context.Context, submitted camera.Event) {
	timer := time.NewTimer(p.options.Config.Recording.PostCapture + 15*time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return
	}
	for attempt := range 5 {
		err := ExtractClipSpan(ctx, p.options.Tracer, p.options.Recorder, submitted, attempt+1)
		if err == nil {
			return
		}
		if attempt == 4 {
			slog.Error("failed to save clip after retries", "event", submitted.ID, "error", err)
			return
		}
		slog.Debug("clip not ready, retrying", "event", submitted.ID, "attempt", attempt+1)
		retryTimer := time.NewTimer(time.Duration(attempt+1) * 30 * time.Second)
		select {
		case <-retryTimer.C:
		case <-ctx.Done():
			retryTimer.Stop()
			return
		}
	}
}

func (p *Processor) handlePresence(ctx context.Context, presence camera.PresenceEvent) {
	if err := p.options.DB.UpdateZonePresence(presence.ZoneID, presence.Label, presence.Type == "zone_enter"); err != nil {
		slog.Error("failed to persist presence event", "zone", presence.ZoneName,
			"label", presence.Label, "error", err)
	}
	if publisher := p.publisher(ctx); publisher != nil {
		var objectName string
		if presence.Type == "zone_enter" {
			var err error
			objectName, err = p.options.DB.LatestObjectNameForZone(presence.ZoneName, presence.Label)
			if err != nil {
				slog.Error("failed to look up latest object name for zone", "zone", presence.ZoneName,
					"label", presence.Label, "error", err)
			}
		}
		SpanPublish(ctx, p.options.Tracer, "mqtt.publish_presence", func() error {
			return publisher.PublishPresence(presence, objectName)
		})
	}
}

func (p *Processor) handleFaces(faceEvent camera.FaceEvent) {
	for _, result := range faceEvent.Results {
		personID, similarity := matchFaceToPerson(p.options.DB, result.Embedding, p.options.FaceRecognizer)
		face := storage.Face{
			EventID: faceEvent.EventID, Camera: faceEvent.Camera,
			Embedding: detect.Float32ToBytes(result.Embedding), CropPath: result.CropPath,
			Confidence: float64(result.Confidence), Timestamp: time.Now(),
		}
		if personID > 0 {
			face.PersonID = &personID
			face.Similarity = &similarity
		}
		faceID, err := p.options.DB.SaveFace(face)
		if err != nil {
			slog.Error("failed to save face", "error", err)
			continue
		}
		if personID == 0 {
			clusterUnmatchedFace(p.options.DB, faceID, result.Embedding, faceEvent.Camera)
			continue
		}

		updatePersonCentroid(p.options.DB, personID, result.Embedding)
		if person, err := p.options.DB.GetPerson(personID); err == nil && person != nil && person.Name != "" {
			_ = p.options.DB.UpdateEventSubLabel(faceEvent.EventID, person.Name)
			if p.options.Server != nil && faceEvent.Kind == camera.EventKindDoorbell {
				p.options.Server.BroadcastDoorbellPersonSSE(faceEvent.Camera, faceEvent.EventID, person.Name)
			}
		}
		slog.Info("face matched to person", "person_id", personID,
			"similarity", fmt.Sprintf("%.3f", similarity), "camera", faceEvent.Camera)
	}
}

func cooldownKey(submitted camera.Event) string {
	return submitted.CameraName + "|" + submitted.Label + "|" + submitted.ZoneName
}

// DoorbellClipWindow returns the effective post-press event window.
func DoorbellClipWindow(cfg *config.Config, cameraName string) time.Duration {
	seconds := cfg.Doorbell.ClipSeconds
	for i := range cfg.Cameras {
		if cfg.Cameras[i].Name == cameraName {
			seconds = cfg.Cameras[i].EffectiveDoorbellClipSeconds(cfg.Doorbell.ClipSeconds)
			break
		}
	}
	if seconds <= 0 {
		seconds = 15
	}
	return time.Duration(seconds) * time.Second
}
