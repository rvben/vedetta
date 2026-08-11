package camera

import (
	"context"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/config"
)

func TestStopCamera(t *testing.T) {
	m := &Manager{
		cameras:     make(map[string]*Camera),
		cancelFuncs: make(map[string]context.CancelFunc),
		order:       []string{"test-cam"},
	}
	m.cameras["test-cam"] = &Camera{config: config.CameraConfig{Name: "test-cam"}}

	if err := m.StopCamera("nonexistent"); err == nil {
		t.Fatal("expected error for nonexistent camera")
	}

	if err := m.StopCamera("test-cam"); err == nil {
		t.Fatal("expected error for camera without cancel func")
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.cancelFuncs["test-cam"] = cancel

	if err := m.StopCamera("test-cam"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("context not cancelled after StopCamera")
	}

	if _, ok := m.cancelFuncs["test-cam"]; ok {
		t.Fatal("cancel func should be removed after StopCamera")
	}

	if err := m.StopCamera("test-cam"); err == nil {
		t.Fatal("expected error for already-stopped camera")
	}
}

// Manager.Start calls cam.Start sequentially inside initSubsystems before
// the API is marked ready. A snapshot path on a stalled volume must not
// block the loop, or one camera bricks the entire NVR at HTTP 503.
func TestManagerStartNotGatedByBlockingSnapshot(t *testing.T) {
	cam0 := newTestCamera(config.CameraConfig{Name: "cam0", URL: "rtsp://localhost/0"}, nil)
	cam0.latestSnapshotPath = blockingSnapshotFIFO(t)
	cam1 := newTestCamera(config.CameraConfig{Name: "cam1", URL: "rtsp://localhost/1"}, nil)

	m := &Manager{
		cameras:     map[string]*Camera{"cam0": cam0, "cam1": cam1},
		cancelFuncs: make(map[string]context.CancelFunc),
		order:       []string{"cam0", "cam1"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		m.Start(ctx, nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Manager.Start did not return: one camera's stalled snapshot disk blocked all camera startup and NVR readiness")
	}
}

func TestIsStopped(t *testing.T) {
	m := &Manager{
		cameras:     make(map[string]*Camera),
		cancelFuncs: make(map[string]context.CancelFunc),
		order:       []string{"cam1"},
	}
	m.cameras["cam1"] = &Camera{config: config.CameraConfig{Name: "cam1"}}

	if !m.IsStopped("cam1") {
		t.Fatal("camera without cancel func should be stopped")
	}

	_, cancel := context.WithCancel(context.Background())
	m.cancelFuncs["cam1"] = cancel

	if m.IsStopped("cam1") {
		t.Fatal("camera with cancel func should not be stopped")
	}

	if m.IsStopped("nonexistent") {
		t.Fatal("nonexistent camera should not report as stopped")
	}
}

// An on-demand camera satisfies Camera.Status's sleeping condition (on_demand
// set, no recent frames) whether it is resting between events or an operator
// stopped it. Only the manager can tell those apart, so it must resolve the
// overlap: reporting both leaves every consumer to invent its own precedence,
// and the ones that get it wrong caption a deliberately-stopped camera as a
// battery camera that will wake itself on motion.
func TestStoppedOnDemandCameraIsNotAlsoSleeping(t *testing.T) {
	cfg := config.CameraConfig{Name: "battery-cam", OnDemand: true}
	m := &Manager{
		cameras:     map[string]*Camera{"battery-cam": {config: cfg}},
		cancelFuncs: make(map[string]context.CancelFunc),
		order:       []string{"battery-cam"},
	}

	statuses := m.CameraStatuses()
	if len(statuses) != 1 {
		t.Fatalf("expected 1 status, got %d", len(statuses))
	}
	if !statuses[0].Stopped {
		t.Fatal("camera without a cancel func should report stopped")
	}
	if statuses[0].Sleeping {
		t.Error("stopped on-demand camera reported sleeping: stopped and sleeping must be mutually exclusive")
	}

	_, cancel := context.WithCancel(context.Background())
	m.cancelFuncs["battery-cam"] = cancel

	statuses = m.CameraStatuses()
	if statuses[0].Stopped {
		t.Fatal("running camera should not report stopped")
	}
	if !statuses[0].Sleeping {
		t.Error("running on-demand camera with no frames should report sleeping")
	}
}
