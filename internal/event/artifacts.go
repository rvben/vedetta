package event

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/image/draw"

	"github.com/rvben/vedetta/internal/camera"
)

// mqttSnapshotMaxWidth bounds the width of snapshots published over MQTT.
// Camera frames are multi-megapixel (2560x1920 is typical) and encode to
// roughly 1 MB of JPEG. PublishSnapshot sends that payload twice, retained, so
// a full-resolution frame pushes ~2 MB into the broker socket for one event.
// That overruns the TCP send buffer and blocks paho's single writer goroutine,
// which also carries the keepalive PINGREQ, so the broker drops the session on
// keepalive timeout and the last-will marks the NVR offline. Subscribers render
// these as dashboard tiles and phone notifications; full resolution stays
// available over the HTTP snapshot API.
const mqttSnapshotMaxWidth = 640

// EmitEventArtifacts persists an event snapshot, publishes the event and image
// to MQTT, and enqueues alert notifications. Callers run it asynchronously
// after the event itself has been stored successfully.
func EmitEventArtifacts(ctx context.Context, tracer trace.Tracer,
	saver SnapshotSaver, publisher ArtifactPublisher, notifier Enqueuer,
	snapshotQuality int, submitted camera.Event) {

	if saver != nil && submitted.SnapshotImage != nil && submitted.SnapshotPath != "" {
		_, snapshotSpan := tracer.Start(ctx, "snapshot.save")
		resolved, err := saver.SaveEventSnapshot(submitted, submitted.SnapshotImage, submitted.SnapshotPath)
		if err != nil {
			snapshotSpan.RecordError(err)
			snapshotSpan.SetStatus(codes.Error, "save snapshot")
			slog.Error("failed to save event snapshot", "event", submitted.ID, "error", err)
		} else {
			submitted.SnapshotPath = resolved
			submitted.SnapshotAvailable = true
		}
		snapshotSpan.End()
	}

	if publisher != nil {
		mqttCtx, mqttSpan := tracer.Start(ctx, "mqtt.publish")

		_, eventSpan := tracer.Start(mqttCtx, "mqtt.publish_event")
		if err := publisher.PublishEvent(submitted, nil); err != nil {
			eventSpan.RecordError(err)
			eventSpan.SetStatus(codes.Error, "publish event")
			mqttSpan.SetStatus(codes.Error, "publish event")
			slog.Error("failed to publish event", "error", err)
		}
		eventSpan.End()

		mqttImage := submitted.AnnotatedImage
		if mqttImage == nil {
			mqttImage = submitted.SnapshotImage
		}
		// Encoded once and shared by both publishes below: the doorbell topic
		// carries the same frame, and re-encoding a multi-megapixel image costs
		// more than the publish itself.
		var jpegData []byte
		if mqttImage != nil {
			_, encodeSpan := tracer.Start(mqttCtx, "snapshot.encode")
			jpegData = encodeMQTTSnapshot(mqttImage, snapshotQuality)
			encodeSpan.End()
			if jpegData != nil {
				_, snapshotSpan := tracer.Start(mqttCtx, "mqtt.publish_snapshot")
				publisher.PublishSnapshot(submitted.CameraName, submitted.Label, jpegData)
				snapshotSpan.End()
			}
		}

		if submitted.Kind == camera.EventKindDoorbell {
			_, doorbellSpan := tracer.Start(mqttCtx, "mqtt.publish_doorbell")
			publisher.PublishDoorbell(submitted.CameraName, submitted.SubLabel, jpegData)
			doorbellSpan.End()
		}

		mqttSpan.End()
	}

	if notifier != nil && submitted.Category != camera.CategoryDetection {
		// A doorbell press is actionable now, not after the incident quiet
		// period. Enqueue it only after snapshot persistence so the push can
		// carry a valid image. The finalized Activity notification later uses
		// the same replacement tag and therefore does not create a second card.
		if submitted.Kind == camera.EventKindDoorbell {
			if immediate, ok := notifier.(DoorbellEnqueuer); ok {
				immediate.EnqueueDoorbell(submitted)
				return
			}
		}
		// Activity-aware dispatchers notify once after the incident quiet period.
		// Keep event enqueueing for lightweight/custom Enqueuer implementations.
		if _, activityAware := notifier.(ActivityEnqueuer); !activityAware {
			notifier.Enqueue(submitted)
		}
	}
}

// WaitForEmit bounds ordering waits between asynchronous create and end
// publications.
func WaitForEmit(ctx context.Context, done <-chan struct{}, timeout time.Duration) {
	if done == nil {
		return
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	case <-ctx.Done():
	}
}

// ExtractClipSpan records one clip-extraction attempt as a linked root span.
func ExtractClipSpan(ctx context.Context, tracer trace.Tracer, saver ClipSaver, submitted camera.Event, attempt int) error {
	_, span := tracer.Start(ctx, "clip.extract",
		trace.WithNewRoot(),
		trace.WithLinks(trace.Link{SpanContext: trace.SpanContextFromContext(ctx)}),
		trace.WithAttributes(
			attribute.Int("clip.attempt", attempt),
			attribute.String("vedetta.camera", submitted.CameraName),
			attribute.String("vedetta.label", submitted.Label),
		))
	defer span.End()
	stats, err := saver.SaveClip(ctx, submitted)
	span.SetAttributes(
		attribute.Int("clip.segment_count", stats.SegmentCount),
		attribute.Int64("clip.output_bytes", stats.OutputBytes),
		attribute.Int64("clip.duration_ms", stats.ClipDuration.Milliseconds()),
	)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "save clip")
	}
	return err
}

// SpanPublish records a synchronous publish performed by the processor loop.
func SpanPublish(ctx context.Context, tracer trace.Tracer, name string, publish func() error) {
	_, span := tracer.Start(ctx, name)
	defer span.End()
	if err := publish(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, name)
	}
}

func encodeJPEG(img *image.RGBA, quality int) []byte {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		slog.Error("failed to encode JPEG", "error", err)
		return nil
	}
	return buf.Bytes()
}

// encodeMQTTSnapshot encodes img for publication over MQTT, downscaling it to
// mqttSnapshotMaxWidth first so a single event cannot overrun the broker
// connection.
func encodeMQTTSnapshot(img *image.RGBA, quality int) []byte {
	return encodeJPEG(downscaleWidth(img, mqttSnapshotMaxWidth), quality)
}

// downscaleWidth returns img scaled down to maxWidth, preserving aspect ratio.
// Images already at or below maxWidth are returned as-is, so this never
// upscales a low-resolution camera's frame.
func downscaleWidth(img *image.RGBA, maxWidth int) *image.RGBA {
	bounds := img.Bounds()
	if maxWidth < 1 || bounds.Dx() <= maxWidth {
		return img
	}
	height := (bounds.Dy()*maxWidth + bounds.Dx()/2) / bounds.Dx()
	if height < 1 {
		height = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, maxWidth, height))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, bounds, draw.Src, nil)
	return dst
}
