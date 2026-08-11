package main

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/config"
	eventprocessor "github.com/rvben/vedetta/internal/event"
)

// These aliases keep the existing behavioral tests exercising the extracted
// implementation without adding compatibility shims to the production binary.
type (
	clipSaver      = eventprocessor.ClipSaver
	snapshotSaver  = eventprocessor.SnapshotSaver
	eventPublisher = eventprocessor.ArtifactPublisher
)

func emitEventArtifacts(ctx context.Context, tracer trace.Tracer,
	saver snapshotSaver, publisher eventPublisher, notifier eventprocessor.Enqueuer,
	snapshotQuality int, submitted camera.Event) {
	eventprocessor.EmitEventArtifacts(ctx, tracer, saver, publisher, notifier, snapshotQuality, submitted)
}

func waitForEmit(ctx context.Context, done <-chan struct{}, timeout time.Duration) {
	eventprocessor.WaitForEmit(ctx, done, timeout)
}

func extractClipSpan(ctx context.Context, tracer trace.Tracer, saver clipSaver, submitted camera.Event, attempt int) error {
	return eventprocessor.ExtractClipSpan(ctx, tracer, saver, submitted, attempt)
}

func spanPublish(ctx context.Context, tracer trace.Tracer, name string, publish func() error) {
	eventprocessor.SpanPublish(ctx, tracer, name, publish)
}

func doorbellClipWindow(cfg *config.Config, cameraName string) time.Duration {
	return eventprocessor.DoorbellClipWindow(cfg, cameraName)
}
