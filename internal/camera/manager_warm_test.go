package camera

import (
	"context"
	"testing"
)

func TestRunningCameraDetectURLs(t *testing.T) {
	camA := NewTestCamera("a")
	camA.config.URL = "rtsp://192.0.2.60:554/a_sub"
	camB := NewTestCamera("b")
	camB.config.URL = "rtsp://192.0.2.61:554/b_sub"

	m := &Manager{
		cameras: map[string]*Camera{"a": camA, "b": camB},
		running: make(map[string]*runningCamera),
		order:   []string{"a", "b"},
	}
	// Only "a" is running.
	markRunning(m, "a", context.Background(), func() {}, closedChan())

	got := m.RunningCameraDetectURLs()
	if len(got) != 1 || got[0] != "rtsp://192.0.2.60:554/a_sub" {
		t.Fatalf("expected only running camera a's DetectURL, got %v", got)
	}
}
