package detect

import (
	"image"
	"testing"
	"time"
)

func TestSnapshotFaceAnalysisSkipsBusyRecognizer(t *testing.T) {
	fr := &FaceRecognizer{}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	done := make(chan []FaceResult, 1)
	go func() { done <- fr.TryDetectFaces(image.NewRGBA(image.Rect(0, 0, 100, 100)), [4]int{0, 0, 100, 100}) }()
	select {
	case got := <-done:
		if len(got) != 0 {
			t.Fatal("busy face recognizer returned results")
		}
	case <-time.After(time.Second):
		t.Fatal("optional background analysis waited for recognition")
	}
}

func TestSnapshotObjectAnalysisSkipsBusyDetector(t *testing.T) {
	d := &Detector{enabled: true}
	d.mu.Lock()
	defer d.mu.Unlock()
	done := make(chan []Detection, 1)
	go func() { done <- d.TryDetect(image.NewRGBA(image.Rect(0, 0, 100, 100))) }()
	select {
	case got := <-done:
		if len(got) != 0 {
			t.Fatal("busy detector returned results")
		}
	case <-time.After(time.Second):
		t.Fatal("background analysis waited for live detection")
	}
}
