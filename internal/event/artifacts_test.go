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

type blockingCrySnapshotSaver struct {
	entered chan struct{}
	release chan struct{}
}

func (s blockingCrySnapshotSaver) SaveEventSnapshot(camera.Event, *image.RGBA, string) (string, error) {
	close(s.entered)
	<-s.release
	return "/resolved/cry.jpg", nil
}

type earlyCryArtifactNotifier struct {
	artifactNotifier
	early chan camera.Event
}

func (n *earlyCryArtifactNotifier) EnqueueCryEarly(ev camera.Event) func(camera.Event) bool {
	n.early <- ev
	return nil
}

func (n *earlyCryArtifactNotifier) EnqueueCry(ev camera.Event) bool {
	n.immediate = append(n.immediate, ev)
	return true
}

func TestCryPhotoNotificationStartsBeforeSnapshotWriteAndIsNotRepeated(t *testing.T) {
	n := &earlyCryArtifactNotifier{early: make(chan camera.Event, 1)}
	s := blockingCrySnapshotSaver{entered: make(chan struct{}), release: make(chan struct{})}
	ev := camera.Event{ID: "cry-early", Kind: camera.EventKindCry, Category: camera.CategoryAlert,
		SnapshotImage: image.NewRGBA(image.Rect(0, 0, 2, 2)), SnapshotPath: "/pending/cry.jpg", NotificationJPEG: []byte("preview")}
	done := make(chan struct{})
	go func() {
		defer close(done)
		EmitEventArtifacts(context.Background(), noop.NewTracerProvider().Tracer("test"), s, nil, n, 80, ev)
	}()
	select {
	case <-s.entered:
	case <-time.After(time.Second):
		close(s.release)
		t.Fatal("snapshot write did not start")
	}
	select {
	case early := <-n.early:
		if string(early.NotificationJPEG) != "preview" {
			t.Error("early notification lost its inline photo")
		}
	default:
		t.Error("snapshot storage blocked early notification")
	}
	close(s.release)
	<-done
	if len(n.immediate) != 0 || len(n.generic) != 0 {
		t.Fatal("accepted early cry notification repeated after snapshot write")
	}
}
