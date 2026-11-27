package main

import (
	"slices"
	"testing"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/cry"
)

func TestCryRecognitionCamerasSplitsByWhoListens(t *testing.T) {
	off := false
	cameras := []config.CameraConfig{{Name: "nursery"}, {Name: "front_door"}, {Name: "attic", Enabled: &off}}
	cameras[0].Cry.Enabled = true
	cameras[2].Cry.Enabled = true

	listening, notListening := cryRecognitionCameras(cameras)
	if !slices.Equal(listening, []string{"nursery"}) {
		t.Errorf("listening = %v, want [nursery]", listening)
	}
	// A disabled camera's sensor is cleared too, or it lingers in Home
	// Assistant for as long as the broker retains it.
	if !slices.Equal(notListening, []string{"front_door", "attic"}) {
		t.Errorf("not listening = %v, want [front_door attic]", notListening)
	}
}

func TestCryRecognitionState(t *testing.T) {
	if _, ok := cryRecognitionState(camera.CameraStatus{Name: "front_door"}); ok {
		t.Error("a camera without recognition published a state")
	}
	state, ok := cryRecognitionState(camera.CameraStatus{Cry: &cry.Status{State: "unavailable"}})
	if !ok || state != "unavailable" {
		t.Errorf("state = %q, %v, want the monitor's own state", state, ok)
	}
	// The monitor of a stopped camera can still hold its last fault. Published
	// as is, Home Assistant would show a problem on a camera nobody runs.
	state, ok = cryRecognitionState(camera.CameraStatus{Stopped: true, Cry: &cry.Status{State: "unavailable"}})
	if !ok || state != "stopped" {
		t.Errorf("state = %q, %v, want stopped", state, ok)
	}
}
