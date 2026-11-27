package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/cry"
)

type statusRecorder struct {
	mu     sync.Mutex
	status []string
	cry    []string
}

func (r *statusRecorder) PublishCameraStatus(name string, online, stopped, sleeping bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = append(r.status, name)
}

func (r *statusRecorder) PublishCryRecognition(name, state string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cry = append(r.cry, name+"="+state)
}

func (r *statusRecorder) published() (status, cry int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.status), len(r.cry)
}

// MQTT can be switched on while the server runs, which installs the client
// long after startup. Camera status and the crying alert sensor follow from
// the first round after that, including one that falls past the warmup.
func TestCameraStatusIsPublishedToAClientInstalledLater(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var installed *statusRecorder
	rounds := 0
	client := func() cameraStatusSink {
		mu.Lock()
		defer mu.Unlock()
		rounds++
		if installed == nil {
			return nil
		}
		return installed
	}
	statuses := func() []camera.CameraStatus {
		return []camera.CameraStatus{
			{Name: "nursery", Online: true, Cry: &cry.Status{State: "working"}},
			{Name: "hallway", Online: true},
		}
	}
	go publishCameraStatuses(ctx, client, statuses, time.Millisecond, time.Millisecond)

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitFor("the rounds without a client to pass the warmup", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return rounds > cameraStatusWarmupPublishes+2
	})

	rec := &statusRecorder{}
	mu.Lock()
	installed = rec
	mu.Unlock()
	waitFor("a round that publishes to the installed client", func() bool {
		status, cry := rec.published()
		return status >= 2 && cry >= 1
	})

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, entry := range rec.cry {
		if entry != "nursery=working" {
			t.Errorf("crying alert state published as %q, want only nursery=working", entry)
		}
	}
}
