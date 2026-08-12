// Package lifecycle coordinates process shutdown across runtime components.
package lifecycle

import (
	"context"
	"errors"
	"time"
)

// Server is the HTTP shutdown boundary owned by the coordinator.
type Server interface {
	Shutdown(context.Context) error
}

// Recorder is the recording shutdown boundary owned by the coordinator.
type Recorder interface {
	Close()
}

// Options configures a Coordinator.
type Options struct {
	Server          Server
	Recorder        Recorder
	StopBackground  context.CancelFunc
	OnShutdown      func()
	ShutdownTimeout time.Duration
}

// Coordinator waits for a process stop and shuts runtime components down.
type Coordinator struct {
	options Options
}

// New validates and constructs a Coordinator.
func New(options Options) (*Coordinator, error) {
	if options.Server == nil {
		return nil, errors.New("lifecycle coordinator: server is required")
	}
	if options.StopBackground == nil {
		return nil, errors.New("lifecycle coordinator: background stop is required")
	}
	if options.ShutdownTimeout <= 0 {
		return nil, errors.New("lifecycle coordinator: shutdown timeout must be positive")
	}
	return &Coordinator{options: options}, nil
}

// Await blocks until the application context is cancelled, then stops the HTTP
// server, background work, and recorder in order.
func (c *Coordinator) Await(ctx context.Context) error {
	<-ctx.Done()
	if c.options.OnShutdown != nil {
		c.options.OnShutdown()
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), c.options.ShutdownTimeout)
	defer cancelShutdown()
	shutdownErr := c.options.Server.Shutdown(shutdownCtx)
	c.options.StopBackground()
	if c.options.Recorder != nil {
		c.options.Recorder.Close()
	}
	return shutdownErr
}
