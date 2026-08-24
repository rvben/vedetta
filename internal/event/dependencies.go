package event

import (
	"context"
	"image"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/recording"
	"github.com/rvben/vedetta/internal/storage"
)

// Recorder owns snapshot persistence, temporary recording, and clip creation.
type Recorder interface {
	SaveEventSnapshot(event camera.Event, img *image.RGBA, primaryPath string) (string, error)
	SaveClip(ctx context.Context, event camera.Event) (recording.ClipStats, error)
	CameraURL(name string) string
	StartTemporaryRecording(ctx context.Context, cameraName, rtspURL string)
}

// SnapshotSaver is the narrow dependency needed by EmitEventArtifacts.
type SnapshotSaver interface {
	SaveEventSnapshot(event camera.Event, img *image.RGBA, primaryPath string) (string, error)
}

// ClipSaver is the narrow dependency needed by ExtractClipSpan.
type ClipSaver interface {
	SaveClip(ctx context.Context, event camera.Event) (recording.ClipStats, error)
}

// ArtifactPublisher is the MQTT surface used while emitting a new event.
type ArtifactPublisher interface {
	PublishEvent(event camera.Event, matchedObjects []string) error
	PublishSnapshot(cameraName, label string, jpegData []byte)
	PublishDoorbell(cameraName, person string, jpegData []byte)
}

// Publisher is the complete MQTT surface used by event processing.
type Publisher interface {
	ArtifactPublisher
	PublishObjectCount(cameraName, label string, count int) error
	PublishPresence(event camera.PresenceEvent, objectName string) error
	PublishObjectSighting(objectName string, event camera.Event)
}

// Enqueuer accepts alert events for asynchronous notification delivery.
type Enqueuer interface {
	Enqueue(event camera.Event)
}

// ActivityEnqueuer accepts a finalized incident and reports whether the
// non-blocking queue accepted it.
type ActivityEnqueuer interface {
	EnqueueActivity(activity storage.Activity) bool
}

// RuntimeServer is the event processor's UI/status publication surface.
type RuntimeServer interface {
	RecordDoorbellPress(cameraName string)
	BroadcastDoorbellSSE(cameraName, eventID, person string)
	BroadcastDoorbellPersonSSE(cameraName, eventID, person string)
	BroadcastActivitySSE(eventType string, activity storage.Activity)
	PublishDetection(frame camera.DetectionFrame)
}

// CameraLookup resolves the live camera whose track may receive a recognized
// object name.
type CameraLookup interface {
	GetCamera(name string) *camera.Camera
}

// ObjectEmbedder is the inference boundary needed for object recognition.
type ObjectEmbedder interface {
	Embed(frame *image.RGBA, box [4]int) ([]float32, error)
}

// FaceRecognizer provides the configured face-match threshold.
type FaceRecognizer interface {
	MatchThreshold() float64
}
