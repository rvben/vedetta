package camera

import (
	"context"
	"testing"

	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/rtsp"
)

func newTestCamera(cfg config.CameraConfig, hub *rtsp.Hub) *Camera {
	return NewCamera(
		cfg,
		nil,
		config.MotionConfig{PixelThreshold: 25, MinArea: 200, BackgroundAlpha: 0.05, MinRegionScore: 0.02},
		make(chan Event, 1),
		make(chan EventEnd, 1),
		nil,
		hub,
		"",
		85,
		"",
		nil,
		nil,
		"",
		nil,
	)
}

func TestSnapshotRGB24_NoFrame(t *testing.T) {
	cam := newTestCamera(config.CameraConfig{
		Name:   "test",
		Detect: config.DetectStreamConfig{Width: 64, Height: 64, FPS: 5},
	}, nil)

	dst := make([]byte, 64*64*3)
	_, _, ok := cam.SnapshotRGB24(dst)
	if ok {
		t.Fatal("expected ok=false when no frame available")
	}
}

func TestSnapshotRGB24_CopiesFrame(t *testing.T) {
	cam := newTestCamera(config.CameraConfig{
		Name:   "test",
		Detect: config.DetectStreamConfig{Width: 4, Height: 4, FPS: 5},
	}, nil)

	frameSize := 4 * 4 * 3
	frame := make([]byte, frameSize)
	for i := range frame {
		frame[i] = byte(i % 256)
	}

	cam.mu.Lock()
	cam.rawFrame = make([]byte, frameSize)
	copy(cam.rawFrame, frame)
	cam.frameW = 4
	cam.frameH = 4
	cam.mu.Unlock()

	dst := make([]byte, frameSize)
	w, h, ok := cam.SnapshotRGB24(dst)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if w != 4 || h != 4 {
		t.Fatalf("expected 4x4, got %dx%d", w, h)
	}
	for i := range frame {
		if dst[i] != frame[i] {
			t.Fatalf("byte %d: got %d, want %d", i, dst[i], frame[i])
		}
	}
}

func TestSnapshotRGB24_DstTooSmall(t *testing.T) {
	cam := newTestCamera(config.CameraConfig{
		Name:   "test",
		Detect: config.DetectStreamConfig{Width: 4, Height: 4, FPS: 5},
	}, nil)

	frameSize := 4 * 4 * 3
	cam.mu.Lock()
	cam.rawFrame = make([]byte, frameSize)
	cam.frameW = 4
	cam.frameH = 4
	cam.mu.Unlock()

	dst := make([]byte, 10) // too small
	_, _, ok := cam.SnapshotRGB24(dst)
	if ok {
		t.Fatal("expected ok=false when dst too small")
	}
}

func TestFrameSize(t *testing.T) {
	cam := newTestCamera(config.CameraConfig{
		Name:   "test",
		Detect: config.DetectStreamConfig{Width: 320, Height: 240, FPS: 5},
	}, nil)

	expected := 320 * 240 * 3
	if got := cam.FrameSize(); got != expected {
		t.Fatalf("FrameSize() = %d, want %d", got, expected)
	}
}

func TestIsOnline_NoHub(t *testing.T) {
	cam := newTestCamera(config.CameraConfig{
		Name: "test",
		URL:  "rtsp://localhost/test",
	}, nil)

	if cam.IsOnline() {
		t.Error("expected IsOnline=false with nil hub")
	}
}

func TestIsOnline_NoSource(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := rtsp.NewHub(ctx)
	defer hub.Close()

	cam := newTestCamera(config.CameraConfig{
		Name: "test",
		URL:  "rtsp://localhost/test",
	}, hub)

	// No source created for this URL yet
	if cam.IsOnline() {
		t.Error("expected IsOnline=false when no source exists")
	}
}

func TestStatus_NoHub(t *testing.T) {
	cam := newTestCamera(config.CameraConfig{
		Name: "test",
		URL:  "rtsp://localhost/test",
	}, nil)

	st := cam.Status()
	if st.Online {
		t.Error("expected Online=false with nil hub")
	}
	if st.Name != "test" {
		t.Errorf("Name = %q, want %q", st.Name, "test")
	}
}

func TestManager_AddCamera(t *testing.T) {
	events := make(chan Event, 10)
	eventEnds := make(chan EventEnd, 10)
	presenceEvents := make(chan PresenceEvent, 10)
	faceEvents := make(chan FaceEvent, 10)
	m := NewManager(nil, nil, config.MotionConfig{}, events, eventEnds, presenceEvents, nil, "", 85, "", nil, faceEvents, "", nil)
	if len(m.ListCameras()) != 0 {
		t.Fatal("expected 0 cameras initially")
	}
	cfg := config.CameraConfig{Name: "test_cam", URL: "rtsp://localhost/stream"}
	m.AddCamera(cfg)
	names := m.ListCameras()
	if len(names) != 1 || names[0] != "test_cam" {
		t.Errorf("expected [test_cam], got %v", names)
	}
	// Adding same name again should be a no-op
	m.AddCamera(cfg)
	if len(m.ListCameras()) != 1 {
		t.Error("duplicate add should be ignored")
	}
}

func TestProcessFrame_PreservesDetectorDegradedState(t *testing.T) {
	cam := newTestCamera(config.CameraConfig{
		Name:   "test",
		URL:    "rtsp://localhost/test",
		Detect: config.DetectStreamConfig{Width: 4, Height: 4, FPS: 5},
	}, nil)

	if st := cam.Status(); !st.Degraded || st.DegradedReason != "object detector unavailable" {
		t.Fatalf("initial status = %+v, want degraded object detector unavailable", st)
	}

	cam.processFrame(make([]byte, 4*4*3), 4, 4)

	if st := cam.Status(); !st.Degraded || st.DegradedReason != "object detector unavailable" {
		t.Fatalf("status after frame = %+v, want degraded object detector unavailable", st)
	}
}
