package event

import (
	"context"
	"image"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace/noop"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/storage"
)

type artifactSnapshotSaver struct{}

func (artifactSnapshotSaver) SaveEventSnapshot(camera.Event, *image.RGBA, string) (string, error) {
	return "/resolved/ring.jpg", nil
}

type artifactNotifier struct {
	immediate []camera.Event
	generic   []camera.Event
}

func (n *artifactNotifier) Enqueue(ev camera.Event) { n.generic = append(n.generic, ev) }
func (n *artifactNotifier) EnqueueDoorbell(ev camera.Event) bool {
	n.immediate = append(n.immediate, ev)
	return true
}
func (n *artifactNotifier) EnqueueActivity(storage.Activity) bool { return true }

func TestEmitEventArtifacts_DoorbellEnqueuesImmediatelyAfterSnapshot(t *testing.T) {
	n := &artifactNotifier{}
	ev := camera.Event{
		ID: "ring-1", CameraName: "front_door", Label: "doorbell",
		Kind: camera.EventKindDoorbell, Category: camera.CategoryAlert,
		Timestamp: time.Now(), SnapshotImage: image.NewRGBA(image.Rect(0, 0, 2, 2)),
		SnapshotPath: "/pending/ring.jpg",
	}

	EmitEventArtifacts(context.Background(), noop.NewTracerProvider().Tracer("test"),
		artifactSnapshotSaver{}, nil, n, 80, ev)

	if len(n.immediate) != 1 {
		t.Fatalf("immediate notifications = %d, want 1", len(n.immediate))
	}
	if !n.immediate[0].SnapshotAvailable || n.immediate[0].SnapshotPath != "/resolved/ring.jpg" {
		t.Fatalf("immediate ring did not receive persisted snapshot: %+v", n.immediate[0])
	}
	if len(n.generic) != 0 {
		t.Fatalf("generic notifications = %d, want 0", len(n.generic))
	}
}
