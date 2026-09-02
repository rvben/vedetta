package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/rvben/vedetta/internal/lifecycle"
)

const gracefulShutdownTimeout = 5 * time.Second

// awaitShutdown blocks until the process is asked to stop. drained, when
// non-nil, is closed once the event processor and the goroutines it detached
// have finished; waiting for it keeps that work from outliving the subsystems
// it uses.
func awaitShutdown(ctx context.Context, cancel context.CancelFunc, server lifecycle.Server, recorder lifecycle.Recorder, drained <-chan struct{}) {
	coordinator, err := lifecycle.New(lifecycle.Options{
		Server:          server,
		Recorder:        recorder,
		StopBackground:  cancel,
		Drain:           drainFunc(drained),
		OnShutdown:      func() { slog.Info("shutting down") },
		ShutdownTimeout: gracefulShutdownTimeout,
	})
	if err != nil {
		panic(err)
	}

	if err := coordinator.Await(ctx); err != nil {
		slog.Error("HTTP server shutdown error", "error", err)
	}
	slog.Info("shutdown complete")
}

// drainFunc adapts a completion channel to the coordinator's drain hook. A
// drain that outlives its context is reported rather than waited on forever:
// the process is going away either way, and the log line is what tells an
// operator which shutdown was not clean.
func drainFunc(drained <-chan struct{}) func(context.Context) {
	if drained == nil {
		return nil
	}
	return func(ctx context.Context) {
		select {
		case <-drained:
		case <-ctx.Done():
			slog.Warn("event processor did not drain before the shutdown deadline",
				"timeout", gracefulShutdownTimeout)
		}
	}
}
