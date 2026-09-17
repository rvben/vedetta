package snapshot

import (
	"image"
	"image/color"
	"sync"
	"testing"
	"time"
)

func visitorFrame(sharp bool) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 160, 160))
	for y := 0; y < 160; y++ {
		for x := 0; x < 160; x++ {
			v := uint8(128)
			if sharp {
				if (x/5+y/5)%2 == 0 {
					v = 64
				} else {
					v = 192
				}
			}
			img.SetRGBA(x, y, color.RGBA{v, v, v, 255})
		}
	}
	return img
}
func visitorSubject(face bool) Subject {
	return Subject{Box: image.Rect(20, 20, 140, 140), Confidence: 0.9, Face: face, Frontalness: 1}
}

func TestDoorbellPrefersVisibleFaceBeforePress(t *testing.T) {
	at := time.Now()
	earlier, later := visitorFrame(true), visitorFrame(true)
	var b DoorbellBuffer
	b.Add(earlier, at.Add(-500*time.Millisecond), []Subject{visitorSubject(true)})
	b.Add(later, at, []Subject{visitorSubject(false)})
	got, stamp, kind := b.Best(at)
	if got != earlier || !stamp.Equal(at.Add(-500*time.Millisecond)) || kind != "face" {
		t.Fatal("should select the visible face from half a second before the press")
	}
}

func TestDoorbellPrefersSharpVisitorAndIgnoresBackground(t *testing.T) {
	at := time.Now()
	clear := visitorFrame(false)
	blur := visitorFrame(true)
	// The clear subject is surrounded by a blank wall; the blurred subject by
	// crisp background detail. Only the visitor's crop should affect sharpness.
	box := image.Rect(40, 40, 120, 120)
	for y := box.Min.Y; y < box.Max.Y; y++ {
		for x := box.Min.X; x < box.Max.X; x++ {
			v := uint8(64)
			if (x/5+y/5)%2 == 0 {
				v = 192
			}
			clear.SetRGBA(x, y, color.RGBA{v, v, v, 255})
			blur.SetRGBA(x, y, color.RGBA{128, 128, 128, 255})
		}
	}
	subject := Subject{Box: box, Confidence: 0.9, Face: true, Frontalness: 1}
	var b DoorbellBuffer
	b.Add(clear, at.Add(-time.Second), []Subject{subject})
	b.Add(blur, at, []Subject{subject})
	if got, _, _ := b.Best(at); got != clear {
		t.Fatal("sharp background won over the sharper visitor")
	}
}

func TestDoorbellWindowAndRecency(t *testing.T) {
	at := time.Now()
	old, current, future := visitorFrame(true), visitorFrame(false), visitorFrame(true)
	var b DoorbellBuffer
	b.Add(old, at.Add(-DoorbellWindow-time.Nanosecond), []Subject{visitorSubject(true)})
	b.Add(current, at.Add(-100*time.Millisecond), []Subject{visitorSubject(false)})
	b.Add(future, at.Add(time.Millisecond), []Subject{visitorSubject(true)})
	if got, _, _ := b.Best(at); got != current {
		t.Fatal("selected a stale or post-press frame")
	}
	if got, _, _ := b.Best(at.Add(3 * time.Second)); got != nil {
		t.Fatal("expired visitor should fall back to latest camera frame")
	}
	b.Add(old, at.Add(-time.Second), []Subject{visitorSubject(true)})
	b.Add(future, at, []Subject{visitorSubject(true)})
	if got, _, _ := b.Best(at); got != future {
		t.Fatal("equally good recent frame should win")
	}
}

func TestDoorbellNoVisitorAndInvalidSubjects(t *testing.T) {
	var b DoorbellBuffer
	at := time.Now()
	img := visitorFrame(true)
	b.Add(img, at, nil)
	b.Add(img, at, []Subject{{Box: image.Rect(200, 200, 210, 210), Confidence: 1}, {Box: img.Bounds(), Confidence: 0.1}})
	b.Add(nil, at, []Subject{visitorSubject(true)})
	if got, _, _ := b.Best(at); got != nil {
		t.Fatal("empty/invalid subject should use camera fallback")
	}
}

func TestDoorbellBoundsMemoryAndOutOfOrderWork(t *testing.T) {
	at := time.Now()
	var b DoorbellBuffer
	img := image.NewRGBA(image.Rect(0, 0, 2560, 1920))
	for i := 0; i < 20; i++ {
		b.Add(img, at.Add(time.Duration(i)*time.Millisecond), []Subject{visitorSubject(true)})
	}
	if len(b.frames) > doorbellMaxFrames || b.bytes > doorbellMaxBytes {
		t.Fatal("buffer exceeded memory/count bound")
	}
	b.Add(visitorFrame(true), at.Add(-3*time.Second), []Subject{visitorSubject(true)})
	for _, f := range b.frames {
		if f.at.Before(at.Add(-DoorbellWindow)) {
			t.Fatal("stale analysis re-entered buffer")
		}
	}
	b.Clear()
	if len(b.frames) != 0 || b.bytes != 0 {
		t.Fatal("buffer retained frames after clearing")
	}
}

func TestDoorbellFrontalFacePreference(t *testing.T) {
	img := visitorFrame(true)
	front, side := visitorSubject(true), visitorSubject(true)
	side.Frontalness = 0
	if doorbellSubjectScore(img, front) <= doorbellSubjectScore(img, side) {
		t.Fatal("frontal face should score above equivalent side view")
	}
}

func TestDoorbellConcurrentCaptureSelectionAndClear(t *testing.T) {
	var b DoorbellBuffer
	img := visitorFrame(true)
	var workers sync.WaitGroup
	for i := 0; i < 3; i++ {
		workers.Go(func() {
			for j := 0; j < 30; j++ {
				at := time.Now()
				b.Add(img, at, []Subject{visitorSubject(true)})
				b.Best(at)
				if j%7 == 0 {
					b.Clear()
				}
			}
		})
	}
	workers.Wait()
}

func BenchmarkDoorbellSelection(b *testing.B) {
	var buffer DoorbellBuffer
	at := time.Now()
	img := visitorFrame(true)
	for i := 0; i < 8; i++ {
		buffer.Add(img, at.Add(-time.Duration(i)*200*time.Millisecond), []Subject{visitorSubject(true)})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		buffer.Best(at)
	}
}
