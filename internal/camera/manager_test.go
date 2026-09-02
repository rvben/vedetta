package camera

import (
	"context"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/config"
)

// markRunning registers name as started with a controllable stopped channel,
// standing in for a camera whose goroutines are still live.
func markRunning(m *Manager, name string, ctx context.Context, cancel context.CancelFunc, stopped <-chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running == nil {
		m.running = make(map[string]*runningCamera)
	}
	m.running[name] = &runningCamera{ctx: ctx, cancel: cancel, stopped: stopped}
}

func closedChan() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func TestStopCamera(t *testing.T) {
	m := &Manager{
		cameras: make(map[string]*Camera),
		running: make(map[string]*runningCamera),
		order:   []string{"test-cam"},
	}
	m.cameras["test-cam"] = &Camera{config: config.CameraConfig{Name: "test-cam"}}

	if err := m.StopCamera("nonexistent"); err == nil {
		t.Fatal("expected error for nonexistent camera")
	}

	if err := m.StopCamera("test-cam"); err == nil {
		t.Fatal("expected error for camera that was never started")
	}

	ctx, cancel := context.WithCancel(context.Background())
	markRunning(m, "test-cam", ctx, cancel, closedChan())

	if err := m.StopCamera("test-cam"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("context not cancelled after StopCamera")
	}

	if _, ok := m.running["test-cam"]; ok {
		t.Fatal("running entry should be removed after StopCamera")
	}

	if err := m.StopCamera("test-cam"); err == nil {
		t.Fatal("expected error for already-stopped camera")
	}
}

// A stop that returns before the camera's goroutines have finished lets the
// caller restart the camera, or report it stopped, while the old RTSP consumer
// is still attached to the hub.
func TestStopCameraWaitsForTheCameraToFinish(t *testing.T) {
	m := &Manager{
		cameras: map[string]*Camera{"cam": {config: config.CameraConfig{Name: "cam"}}},
		running: make(map[string]*runningCamera),
		order:   []string{"cam"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stopped := make(chan struct{})
	go func() {
		<-ctx.Done()
		time.Sleep(150 * time.Millisecond)
		close(stopped)
	}()
	markRunning(m, "cam", ctx, cancel, stopped)

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		if err := m.StopCamera("cam"); err != nil {
			t.Errorf("StopCamera: %v", err)
		}
	}()

	select {
	case <-returned:
		t.Fatal("StopCamera returned while the camera goroutines were still running")
	case <-time.After(50 * time.Millisecond):
	}

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("StopCamera did not return after the camera finished")
	}
}

// A camera wedged in a blocking read must not hold the stop forever: the caller
// is usually an HTTP request, and the manager lock is shared with every other
// operation.
func TestStopCameraIsBoundedWhenTheCameraNeverFinishes(t *testing.T) {
	m := &Manager{
		cameras: map[string]*Camera{"cam": {config: config.CameraConfig{Name: "cam"}}},
		running: make(map[string]*runningCamera),
		order:   []string{"cam"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	markRunning(m, "cam", ctx, cancel, make(chan struct{}))

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := m.StopCamera("cam"); err != nil {
			t.Errorf("StopCamera: %v", err)
		}
	}()

	select {
	case <-done:
	case <-time.After(cameraStopTimeout + 5*time.Second):
		t.Fatal("StopCamera never returned for a camera that never finished")
	}

	// The manager must be usable again, not stuck behind the wedged camera.
	if m.IsStopped("cam") != true {
		t.Fatal("camera should report stopped after StopCamera returned")
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
		cameras: map[string]*Camera{"cam0": cam0, "cam1": cam1},
		running: make(map[string]*runningCamera),
		order:   []string{"cam0", "cam1"},
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
		cameras: make(map[string]*Camera),
		running: make(map[string]*runningCamera),
		order:   []string{"cam1"},
	}
	m.cameras["cam1"] = &Camera{config: config.CameraConfig{Name: "cam1"}}

	if !m.IsStopped("cam1") {
		t.Fatal("camera that was never started should be stopped")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	markRunning(m, "cam1", ctx, cancel, closedChan())

	if m.IsStopped("cam1") {
		t.Fatal("started camera should not be stopped")
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
		cameras: map[string]*Camera{"battery-cam": {config: cfg}},
		running: make(map[string]*runningCamera),
		order:   []string{"battery-cam"},
	}

	statuses := m.CameraStatuses()
	if len(statuses) != 1 {
		t.Fatalf("expected 1 status, got %d", len(statuses))
	}
	if !statuses[0].Stopped {
		t.Fatal("camera that was never started should report stopped")
	}
	if statuses[0].Sleeping {
		t.Error("stopped on-demand camera reported sleeping: stopped and sleeping must be mutually exclusive")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	markRunning(m, "battery-cam", ctx, cancel, closedChan())

	statuses = m.CameraStatuses()
	if statuses[0].Stopped {
		t.Fatal("running camera should not report stopped")
	}
	if !statuses[0].Sleeping {
		t.Error("running on-demand camera with no frames should report sleeping")
	}
}
