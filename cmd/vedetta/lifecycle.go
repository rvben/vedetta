package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/rvben/vedetta/internal/lifecycle"
)

const gracefulShutdownTimeout = 5 * time.Second

func awaitShutdown(ctx context.Context, cancel context.CancelFunc, server lifecycle.Server, recorder lifecycle.Recorder) {
	coordinator, err := lifecycle.New(lifecycle.Options{
		Server:          server,
		Recorder:        recorder,
		StopBackground:  cancel,
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
