package event

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace/noop"

	"github.com/rvben/vedetta/internal/camera"
)

// recordingPublisher captures the exact payloads handed to MQTT.
type recordingPublisher struct {
	snapshots [][]byte
	doorbells [][]byte
}

func (p *recordingPublisher) PublishEvent(camera.Event, []string) error { return nil }
func (p *recordingPublisher) PublishSnapshot(_, _ string, jpegData []byte) {
	p.snapshots = append(p.snapshots, jpegData)
}
func (p *recordingPublisher) PublishDoorbell(_, _ string, jpegData []byte) {
	p.doorbells = append(p.doorbells, jpegData)
}

// cameraFrame builds an image the size a real camera produces, with varied
// content so it does not compress down to nothing and mask a size regression.
func cameraFrame(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{
				R: uint8((x * 7) % 256),
				G: uint8((y * 13) % 256),
				B: uint8((x ^ y) % 256),
				A: 255,
			})
		}
	}
	return img
}

func decodeSize(t *testing.T, data []byte) (int, int) {
	t.Helper()
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("published payload is not decodable JPEG: %v", err)
	}
	return cfg.Width, cfg.Height
}

// The published snapshot goes out twice, retained, on every event. At full
// camera resolution that is ~2 MB pushed into the broker socket per event,
// which overruns the TCP send buffer, blocks paho's writer goroutine, starves
// the keepalive it shares, and gets the session dropped. The payload that
// reaches MQTT must therefore be bounded regardless of the camera's resolution.
func TestEmitEventArtifacts_BoundsPublishedSnapshotResolution(t *testing.T) {
	pub := &recordingPublisher{}
	ev := camera.Event{
		ID: "e-1", CameraName: "front_door", Label: "person",
		Category: camera.CategoryDetection, Timestamp: time.Now(),
		SnapshotImage: cameraFrame(2560, 1920),
	}

	EmitEventArtifacts(context.Background(), noop.NewTracerProvider().Tracer("test"),
		nil, pub, nil, 85, ev)

	if len(pub.snapshots) != 1 {
		t.Fatalf("PublishSnapshot called %d times, want 1", len(pub.snapshots))
	}
	w, h := decodeSize(t, pub.snapshots[0])
	if w > mqttSnapshotMaxWidth {
		t.Errorf("published snapshot is %dx%d, want width <= %d: a full-resolution frame overruns the broker connection",
			w, h, mqttSnapshotMaxWidth)
	}
	// 2560x1920 is 4:3; the bound must preserve that rather than squash it.
	if wantH := mqttSnapshotMaxWidth * 1920 / 2560; h != wantH {
		t.Errorf("published snapshot is %dx%d, want height %d to preserve aspect ratio", w, h, wantH)
	}
}

// A low-resolution camera must not be upscaled: that would inflate the payload
// this bound exists to shrink.
func TestEmitEventArtifacts_LeavesSmallFramesAtNativeResolution(t *testing.T) {
	pub := &recordingPublisher{}
	ev := camera.Event{
		ID: "e-2", CameraName: "garage", Label: "person",
		Category: camera.CategoryDetection, Timestamp: time.Now(),
		SnapshotImage: cameraFrame(320, 240),
	}

	EmitEventArtifacts(context.Background(), noop.NewTracerProvider().Tracer("test"),
		nil, pub, nil, 85, ev)

	if len(pub.snapshots) != 1 {
		t.Fatalf("PublishSnapshot called %d times, want 1", len(pub.snapshots))
	}
	if w, h := decodeSize(t, pub.snapshots[0]); w != 320 || h != 240 {
		t.Errorf("published snapshot is %dx%d, want 320x240 unchanged", w, h)
	}
}

// Bounding the resolution is only worth anything if it actually shrinks the
// bytes on the wire. cameraFrame is deliberately high-entropy, so these sizes
// are a worst case: a real camera frame compresses several times smaller again.
func TestEncodeMQTTSnapshot_ShrinksPayloadWellBelowTheStallThreshold(t *testing.T) {
	frame := cameraFrame(2560, 1920)

	full := encodeJPEG(frame, 85)
	bounded := encodeMQTTSnapshot(frame, 85)

	if len(bounded)*8 > len(full) {
		t.Errorf("bounded encode is %d bytes against %d unbounded, want at least an 8x reduction",
			len(bounded), len(full))
	}
	// PublishSnapshot sends the payload twice back to back. The observed stall
	// froze with 860,856 bytes queued on the broker socket, so both copies
	// together have to stay comfortably under that to keep the write from
	// blocking in the first place.
	const inFlightBudget = 512 * 1024
	if 2*len(bounded) > inFlightBudget {
		t.Errorf("two published copies total %d bytes, want <= %d to stay below the level at which the socket write stalled",
			2*len(bounded), inFlightBudget)
	}
}

// The doorbell topic carries the same frame as the snapshot topic, so it is
// subject to the same bound and receives the same shared encode.
func TestEmitEventArtifacts_DoorbellReusesBoundedSnapshot(t *testing.T) {
	pub := &recordingPublisher{}
	ev := camera.Event{
		ID: "ring-1", CameraName: "front_door", Label: "doorbell",
		Kind: camera.EventKindDoorbell, Category: camera.CategoryAlert,
		Timestamp: time.Now(), SnapshotImage: cameraFrame(2560, 1920),
	}

	EmitEventArtifacts(context.Background(), noop.NewTracerProvider().Tracer("test"),
		nil, pub, nil, 85, ev)

	if len(pub.doorbells) != 1 {
		t.Fatalf("PublishDoorbell called %d times, want 1", len(pub.doorbells))
	}
	if w, h := decodeSize(t, pub.doorbells[0]); w > mqttSnapshotMaxWidth {
		t.Errorf("doorbell payload is %dx%d, want width <= %d", w, h, mqttSnapshotMaxWidth)
	}
	if !bytes.Equal(pub.snapshots[0], pub.doorbells[0]) {
		t.Error("doorbell and snapshot payloads differ, want the single shared encode")
	}
}

// An event with no image must still publish the doorbell press, carrying no
// payload rather than a bogus one.
func TestEmitEventArtifacts_DoorbellWithoutImagePublishesNoPayload(t *testing.T) {
	pub := &recordingPublisher{}
	ev := camera.Event{
		ID: "ring-2", CameraName: "front_door", Label: "doorbell",
		Kind: camera.EventKindDoorbell, Category: camera.CategoryAlert,
		Timestamp: time.Now(),
	}

	EmitEventArtifacts(context.Background(), noop.NewTracerProvider().Tracer("test"),
		nil, pub, nil, 85, ev)

	if len(pub.snapshots) != 0 {
		t.Fatalf("PublishSnapshot called %d times for an imageless event, want 0", len(pub.snapshots))
	}
	if len(pub.doorbells) != 1 {
		t.Fatalf("PublishDoorbell called %d times, want 1", len(pub.doorbells))
	}
	if pub.doorbells[0] != nil {
		t.Errorf("doorbell payload = %d bytes, want nil for an imageless event", len(pub.doorbells[0]))
	}
}
