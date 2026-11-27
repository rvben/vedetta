package api

import (
	"strings"
	"testing"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/cry"
)

func TestCryRecognitionMetricReportsWhetherACryWouldBeHeard(t *testing.T) {
	var b strings.Builder
	writeCryRecognitionMetric(&b, []camera.CameraStatus{
		{Name: "nursery", Cry: &cry.Status{State: "listening"}},
		{Name: "bedroom", Cry: &cry.Status{State: "unavailable"}},
		{Name: "playroom", Cry: &cry.Status{State: "crying"}},
		{Name: "attic", Cry: &cry.Status{State: "starting"}},
		{Name: "front_door"},
		{Name: "shed", Stopped: true, Cry: &cry.Status{State: "stopped"}},
	})
	body := b.String()
	assertSingleTypeBeforeSamples(t, body, "vedetta_cry_recognition_available", "gauge")
	for sample, present := range map[string]bool{
		`vedetta_cry_recognition_available{camera="nursery"} 1`:  true,
		`vedetta_cry_recognition_available{camera="playroom"} 1`: true,
		`vedetta_cry_recognition_available{camera="bedroom"} 0`:  true,
		// Not yet listening is not yet hearing.
		`vedetta_cry_recognition_available{camera="attic"} 0`: true,
		// No recognition, and stopped by the operator: neither is an outage.
		`camera="front_door"`: false,
		`camera="shed"`:       false,
	} {
		if strings.Contains(body, sample) != present {
			t.Errorf("sample %q present = %v, want %v in:\n%s", sample, !present, present, body)
		}
	}
}

func TestCryRecognitionMetricIsAbsentWithoutListeningCameras(t *testing.T) {
	var b strings.Builder
	writeCryRecognitionMetric(&b, []camera.CameraStatus{
		{Name: "front_door"},
		{Name: "shed", Stopped: true, Cry: &cry.Status{State: "stopped"}},
	})
	if b.Len() != 0 {
		t.Errorf("an empty family was written, which records a series nobody has:\n%s", b.String())
	}
}
