package camera

import (
	"context"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/config"
)

func newHookManager(names ...string) *Manager {
	m := &Manager{
		cameras: make(map[string]*Camera),
		running: make(map[string]*runningCamera),
	}
	for _, name := range names {
		m.cameras[name] = NewTestCamera(name)
		m.order = append(m.order, name)
	}
	return m
}

// Per-camera background work (the ONVIF doorbell subscriber) must stop when its
// camera is stopped. Binding it to the process context instead leaves it
// polling a camera the operator turned off, for the life of the process.
func TestCameraStartHookRunsOnTheCameraContext(t *testing.T) {
	m := newHookManager("front")

	hookCtx := make(chan context.Context, 1)
	m.OnCameraStart(func(ctx context.Context, name string) {
		if name != "front" {
			t.Errorf("hook called for %q, want %q", name, "front")
		}
		hookCtx <- ctx
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.StartCamera(ctx, "front"); err != nil {
		t.Fatalf("StartCamera: %v", err)
	}

	var got context.Context
	select {
	case got = <-hookCtx:
	case <-time.After(2 * time.Second):
		t.Fatal("start hook was never called")
	}

	select {
	case <-got.Done():
		t.Fatal("hook context was already cancelled before the camera was stopped")
	default:
	}

	if err := m.StopCamera("front"); err != nil {
		t.Fatalf("StopCamera: %v", err)
	}
	select {
	case <-got.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("stopping the camera did not cancel the work started for it")
	}
}

// Registration order must not decide coverage: a hook registered after startup
// still has to cover the cameras that are already running.
func TestCameraStartHookCoversAlreadyRunningCameras(t *testing.T) {
	m := newHookManager("front", "back")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, name := range []string{"front", "back"} {
		if err := m.StartCamera(ctx, name); err != nil {
			t.Fatalf("StartCamera(%q): %v", name, err)
		}
	}

	seen := make(chan string, 4)
	m.OnCameraStart(func(_ context.Context, name string) { seen <- name })

	got := map[string]bool{}
	for range 2 {
		select {
		case name := <-seen:
			got[name] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("hook was called for %v, want both cameras", got)
		}
	}
	if !got["front"] || !got["back"] {
		t.Fatalf("hook was called for %v, want both cameras", got)
	}
}

// A duplicate name would overwrite the first camera in the lookup map while
// leaving both entries in the start order, so the survivor would be started
// twice and the first camera's configuration silently dropped.
func TestDuplicateCameraNamesAreRejected(t *testing.T) {
	configs := []config.CameraConfig{
		{Name: "front", URL: "rtsp://192.0.2.10:554/one"},
		{Name: "front", URL: "rtsp://192.0.2.11:554/two"},
	}
	m := NewManager(configs, nil, config.MotionConfig{}, nil, nil, nil, nil, "", 80, "", nil, nil, "", nil, nil)

	if len(m.order) != 1 {
		t.Fatalf("start order = %v, want one entry: a duplicate name must not be started twice", m.order)
	}
	if got := m.GetCamera("front").DetectURL(); got != "rtsp://192.0.2.10:554/one" {
		t.Fatalf("kept camera URL = %q, want the first configured camera", got)
	}
}
