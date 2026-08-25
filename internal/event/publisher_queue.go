package event

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/rvben/vedetta/internal/camera"
)

const mqttPublishQueueCapacity = 256

var errMQTTPublishQueueFull = errors.New("MQTT publish queue full")

type mqttPublishJob struct {
	ctx       context.Context
	operation string
	deliver   func(Publisher) error
}

// mqttPublishDispatcher isolates the event state machine from broker latency.
// One bounded worker preserves enqueue order without allowing a broker outage
// to create an unbounded number of blocked goroutines.
type mqttPublishDispatcher struct {
	provider func() Publisher
	tracer   trace.Tracer
	jobs     chan mqttPublishJob
}

func newMQTTPublishDispatcher(provider func() Publisher, tracer trace.Tracer) *mqttPublishDispatcher {
	return &mqttPublishDispatcher{
		provider: provider,
		tracer:   tracer,
		jobs:     make(chan mqttPublishJob, mqttPublishQueueCapacity),
	}
}

func (d *mqttPublishDispatcher) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-d.jobs:
			publisher := d.provider()
			if publisher == nil {
				continue
			}
			d.deliver(job, publisher)
		}
	}
}

func (d *mqttPublishDispatcher) deliver(job mqttPublishJob, publisher Publisher) {
	ctx := job.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	_, span := d.tracer.Start(ctx, "mqtt.deliver_"+job.operation)
	defer span.End()
	defer func() {
		if recovered := recover(); recovered != nil {
			err := fmt.Errorf("panic: %v", recovered)
			span.RecordError(err)
			span.SetStatus(codes.Error, "MQTT publish panic")
			slog.Error("MQTT publish panicked", "operation", job.operation, "error", err)
		}
	}()

	if err := job.deliver(publisher); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "MQTT publish failed")
		slog.Error("MQTT publish failed", "operation", job.operation, "error", err)
	}
}

func (d *mqttPublishDispatcher) enqueue(ctx context.Context, operation string, deliver func(Publisher) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	job := mqttPublishJob{ctx: ctx, operation: operation, deliver: deliver}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case d.jobs <- job:
		return nil
	default:
		slog.Error("MQTT publish dropped", "operation", operation, "error", errMQTTPublishQueueFull)
		return errMQTTPublishQueueFull
	}
}

// queuedPublisher implements Publisher by copying transient inputs into the
// dispatcher's bounded queue. Large decoded images are explicitly removed from
// queued events; MQTT event JSON never contains them, and retaining them while
// a broker is down would unnecessarily pin hundreds of megabytes of memory.
type queuedPublisher struct {
	ctx        context.Context
	dispatcher *mqttPublishDispatcher
}

func (p *queuedPublisher) PublishEvent(event camera.Event, matchedObjects []string) error {
	event.SnapshotImage = nil
	event.AnnotatedImage = nil
	objects := append([]string(nil), matchedObjects...)
	return p.dispatcher.enqueue(p.ctx, "event", func(publisher Publisher) error {
		return publisher.PublishEvent(event, objects)
	})
}

func (p *queuedPublisher) PublishSnapshot(cameraName, label string, jpegData []byte) {
	data := append([]byte(nil), jpegData...)
	_ = p.dispatcher.enqueue(p.ctx, "snapshot", func(publisher Publisher) error {
		publisher.PublishSnapshot(cameraName, label, data)
		return nil
	})
}

func (p *queuedPublisher) PublishDoorbell(cameraName, person string, jpegData []byte) {
	data := append([]byte(nil), jpegData...)
	_ = p.dispatcher.enqueue(p.ctx, "doorbell", func(publisher Publisher) error {
		publisher.PublishDoorbell(cameraName, person, data)
		return nil
	})
}

func (p *queuedPublisher) PublishObjectCount(cameraName, label string, count int) error {
	return p.dispatcher.enqueue(p.ctx, "object_count", func(publisher Publisher) error {
		return publisher.PublishObjectCount(cameraName, label, count)
	})
}

func (p *queuedPublisher) PublishPresence(presence camera.PresenceEvent, objectName string) error {
	return p.dispatcher.enqueue(p.ctx, "presence", func(publisher Publisher) error {
		return publisher.PublishPresence(presence, objectName)
	})
}

func (p *queuedPublisher) PublishObjectSighting(objectName string, event camera.Event) {
	event.SnapshotImage = nil
	event.AnnotatedImage = nil
	_ = p.dispatcher.enqueue(p.ctx, "object_sighting", func(publisher Publisher) error {
		publisher.PublishObjectSighting(objectName, event)
		return nil
	})
}
