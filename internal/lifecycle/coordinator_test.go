package lifecycle_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/lifecycle"
)

func TestCoordinatorShutsDownWhenApplicationContextIsCancelled(t *testing.T) {
	server := &recordingServer{shutdown: make(chan struct{})}
	recorder := &recordingCloser{closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	coordinator, err := lifecycle.New(lifecycle.Options{
		Server:          server,
		Recorder:        recorder,
		StopBackground:  cancel,
		ShutdownTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cancel()
	if err := coordinator.Await(ctx); err != nil {
		t.Fatalf("Await: %v", err)
	}

	select {
	case <-server.shutdown:
	default:
		t.Fatal("HTTP server was not shut down")
	}
	select {
	case <-recorder.closed:
	default:
		t.Fatal("recorder was not closed")
	}
}

func TestCoordinatorSupportsServerOnlyShutdown(t *testing.T) {
	server := &recordingServer{shutdown: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	coordinator, err := lifecycle.New(lifecycle.Options{
		Server: server, StopBackground: cancel, ShutdownTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cancel()
	if err := coordinator.Await(ctx); err != nil {
		t.Fatalf("Await: %v", err)
	}
	select {
	case <-server.shutdown:
	default:
		t.Fatal("HTTP server was not shut down")
	}
}

func TestCoordinatorFinishesCleanupWhenServerShutdownFails(t *testing.T) {
	wantErr := errors.New("shutdown failed")
	server := &recordingServer{shutdown: make(chan struct{}), err: wantErr}
	recorder := &recordingCloser{closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	coordinator, err := lifecycle.New(lifecycle.Options{
		Server: server, Recorder: recorder, StopBackground: cancel, ShutdownTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cancel()

	if err := coordinator.Await(ctx); !errors.Is(err, wantErr) {
		t.Fatalf("Await error = %v, want %v", err, wantErr)
	}
	select {
	case <-recorder.closed:
	default:
		t.Fatal("recorder was not closed after server shutdown failure")
	}
}

func TestCoordinatorBoundsServerShutdownAndStillClosesRecorder(t *testing.T) {
	server := &blockingServer{}
	recorder := &recordingCloser{closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	coordinator, err := lifecycle.New(lifecycle.Options{
		Server: server, Recorder: recorder, StopBackground: cancel, ShutdownTimeout: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cancel()

	if err := coordinator.Await(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Await error = %v, want %v", err, context.DeadlineExceeded)
	}
	select {
	case <-recorder.closed:
	default:
		t.Fatal("recorder was not closed after shutdown timeout")
	}
}

func TestCoordinatorShutsComponentsDownInSafeOrder(t *testing.T) {
	var order []string
	server := shutdownFunc(func(context.Context) error {
		order = append(order, "server")
		return nil
	})
	recorder := closeFunc(func() { order = append(order, "recorder") })
	drain := func(context.Context) { order = append(order, "drain") }
	ctx, cancelContext := context.WithCancel(context.Background())
	stopBackground := func() {
		order = append(order, "background")
		cancelContext()
	}
	coordinator, err := lifecycle.New(lifecycle.Options{
		Server: server, Recorder: recorder, StopBackground: stopBackground,
		Drain: drain, ShutdownTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cancelContext()

	if err := coordinator.Await(ctx); err != nil {
		t.Fatalf("Await: %v", err)
	}
	// Detached background work uses the recorder, so the drain has to complete
	// before the recorder is closed.
	want := []string{"server", "background", "drain", "recorder"}
	if len(order) != len(want) {
		t.Fatalf("shutdown order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("shutdown order = %v, want %v", order, want)
		}
	}
}

type recordingServer struct {
	shutdown chan struct{}
	err      error
}

func (s *recordingServer) Shutdown(context.Context) error {
	close(s.shutdown)
	return s.err
}

type blockingServer struct{}

func (*blockingServer) Shutdown(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

type shutdownFunc func(context.Context) error

func (fn shutdownFunc) Shutdown(ctx context.Context) error { return fn(ctx) }

type closeFunc func()

func (fn closeFunc) Close() { fn() }

type recordingCloser struct {
	closed chan struct{}
}

func (c *recordingCloser) Close() {
	close(c.closed)
}

// A drain that never completes must not wedge the process. The shutdown timeout
// bounds it and the remaining components still close.
func TestCoordinatorBoundsDrainAndStillClosesRecorder(t *testing.T) {
	server := &recordingServer{shutdown: make(chan struct{})}
	recorder := &recordingCloser{closed: make(chan struct{})}
	drainDeadline := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	coordinator, err := lifecycle.New(lifecycle.Options{
		Server:   server,
		Recorder: recorder,
		Drain: func(drainCtx context.Context) {
			<-drainCtx.Done()
			drainDeadline <- drainCtx.Err()
		},
		StopBackground:  cancel,
		ShutdownTimeout: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cancel()

	if err := coordinator.Await(ctx); err != nil {
		t.Fatalf("Await: %v", err)
	}
	select {
	case drainErr := <-drainDeadline:
		if !errors.Is(drainErr, context.DeadlineExceeded) {
			t.Fatalf("drain context error = %v, want %v", drainErr, context.DeadlineExceeded)
		}
	default:
		t.Fatal("drain was not given a bounded context")
	}
	select {
	case <-recorder.closed:
	default:
		t.Fatal("recorder was not closed after the drain deadline")
	}
}
