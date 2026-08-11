// Package event owns Vedetta's event lifecycle orchestration.
package event

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/detect"
	"github.com/rvben/vedetta/internal/storage"
)

const emitWaitTimeout = 5 * time.Second

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
	options Options
	active  map[string]*activeEvent
	timeout chan string

	objectCounts map[string]map[string]int
	cooldowns    map[string]time.Time
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
	return &Processor{
		options:      options,
		active:       make(map[string]*activeEvent),
		timeout:      make(chan string, 100),
		objectCounts: make(map[string]map[string]int),
		cooldowns:    make(map[string]time.Time),
	}, nil
}

// Run processes events until ctx is cancelled. It blocks for the lifetime of
// the processor so callers can wait for a clean shutdown.
func (p *Processor) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			p.stopActiveTimers()
			return
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
	if saveErr == nil && submitted.Kind == camera.EventKindDoorbell && p.options.Server != nil {
		p.options.Server.RecordDoorbellPress(submitted.CameraName)
		p.options.Server.BroadcastDoorbellSSE(submitted.CameraName, submitted.ID, submitted.SubLabel)
	}

	publisher := p.publisher()
	if publisher != nil && submitted.Kind != camera.EventKindDoorbell {
		if p.objectCounts[submitted.CameraName] == nil {
			p.objectCounts[submitted.CameraName] = make(map[string]int)
		}
		p.objectCounts[submitted.CameraName][submitted.Label]++
		SpanPublish(eventCtx, p.options.Tracer, "mqtt.publish_object_count", func() error {
			return publisher.PublishObjectCount(submitted.CameraName, submitted.Label,
				p.objectCounts[submitted.CameraName][submitted.Label])
		})
	}

	var emitDone chan struct{}
	if saveErr == nil {
		emitDone = make(chan struct{})
		go func(eventCopy camera.Event, done chan struct{}) {
			defer close(done)
			EmitEventArtifacts(eventCtx, p.options.Tracer, p.options.Recorder, publisher,
				p.options.Notifier, p.options.Config.Events.SnapshotQuality, eventCopy)
		}(submitted, emitDone)
	}

	if p.options.ObjectEmbedder != nil && submitted.SnapshotImage != nil {
		go p.recognizeObject(eventCtx, submitted)
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
	}

	WaitForEmit(ctx, active.emitDone, emitWaitTimeout)
	if publisher := p.publisher(); publisher != nil {
		SpanPublish(endCtx, p.options.Tracer, "mqtt.publish_event_end", func() error {
			if err := publisher.PublishEvent(event, nil); err != nil {
				slog.Error("failed to publish event end", "event", event.ID, "error", err)
				return err
			}
			return nil
		})
		if event.Kind != camera.EventKindDoorbell {
			if counts, exists := p.objectCounts[event.CameraName]; exists {
				counts[event.Label]--
				if counts[event.Label] < 0 {
					counts[event.Label] = 0
				}
				SpanPublish(endCtx, p.options.Tracer, "mqtt.publish_object_count", func() error {
					return publisher.PublishObjectCount(event.CameraName, event.Label, counts[event.Label])
				})
			}
		}
	}
	endSpan.End()

	slog.Info("event ended", "event", event.ID, "camera", event.CameraName,
		"label", event.Label, "duration", endTime.Sub(event.Timestamp).Round(time.Second))
	p.cooldowns[cooldownKey(event)] = endTime

	if active.tempCancel != nil {
		temporaryCancel := active.tempCancel
		go func() {
			timer := time.NewTimer(p.options.Config.Recording.PostCapture + 5*time.Second)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
			}
			temporaryCancel()
		}()
	}

	if p.options.Recorder != nil {
		clipCtx := trace.ContextWithSpanContext(ctx, active.rootSpanCtx)
		go p.extractClip(clipCtx, event)
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

func (p *Processor) publisher() Publisher {
	if p.options.Publisher == nil {
		return nil
	}
	return p.options.Publisher()
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
	if publisher := p.publisher(); publisher != nil {
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
	if publisher := p.publisher(); publisher != nil {
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
