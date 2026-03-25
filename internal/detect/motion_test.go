package detect

import (
	"testing"
)

func TestMotionScore_Identical(t *testing.T) {
	frame := make([]byte, 300) // 100 pixels * 3 channels
	for i := range frame {
		frame[i] = 128
	}

	score := MotionScore(frame, frame)
	if score != 0 {
		t.Errorf("expected 0 for identical frames, got %f", score)
	}
}

func TestMotionScore_CompletelyDifferent(t *testing.T) {
	prev := make([]byte, 300)
	curr := make([]byte, 300)
	for i := range curr {
		curr[i] = 255
	}

	score := MotionScore(prev, curr)
	if score < 0.99 {
		t.Errorf("expected ~1.0 for max difference, got %f", score)
	}
}

func TestMotionScore_Empty(t *testing.T) {
	score := MotionScore(nil, nil)
	if score != 0 {
		t.Errorf("expected 0 for empty frames, got %f", score)
	}
}

func TestMotionScore_DifferentLengths(t *testing.T) {
	a := make([]byte, 300)
	b := make([]byte, 600)
	score := MotionScore(a, b)
	if score != 0 {
		t.Errorf("expected 0 for mismatched frames, got %f", score)
	}
}

// makeRGB creates a uniform RGB24 frame of the given color.
func makeRGB(w, h int, r, g, b uint8) []byte {
	frame := make([]byte, w*h*3)
	for i := 0; i < w*h; i++ {
		frame[i*3] = r
		frame[i*3+1] = g
		frame[i*3+2] = b
	}
	return frame
}

// drawRect fills a rectangle in an RGB24 frame with the given color.
func drawRect(frame []byte, w, x1, y1, x2, y2 int, r, g, b uint8) {
	for y := y1; y < y2; y++ {
		for x := x1; x < x2; x++ {
			off := (y*w + x) * 3
			frame[off] = r
			frame[off+1] = g
			frame[off+2] = b
		}
	}
}

func TestMotionDetector_StaticScene(t *testing.T) {
	md := NewMotionDetector(25, 50, 0.05)
	w, h := 100, 100
	frame := makeRGB(w, h, 128, 128, 128)

	// First frame initializes background
	regions := md.Detect(frame, w, h)
	if regions != nil {
		t.Errorf("first frame should return nil, got %d regions", len(regions))
	}

	// Second identical frame should produce no motion
	regions = md.Detect(frame, w, h)
	if len(regions) != 0 {
		t.Errorf("static scene should have 0 regions, got %d", len(regions))
	}
}

func TestMotionDetector_SingleMovingObject(t *testing.T) {
	md := NewMotionDetector(25, 10, 0.01) // low alpha so background stays stable
	w, h := 100, 100

	// Frame 1: uniform background
	bg := makeRGB(w, h, 50, 50, 50)
	md.Detect(bg, w, h)

	// Frame 2: bright rectangle appears
	frame2 := makeRGB(w, h, 50, 50, 50)
	drawRect(frame2, w, 30, 30, 60, 60, 200, 200, 200)
	regions := md.Detect(frame2, w, h)

	if len(regions) != 1 {
		t.Fatalf("expected 1 motion region, got %d", len(regions))
	}

	r := regions[0]
	// The region should roughly cover the rectangle
	if r.Box[0] > 35 || r.Box[1] > 35 || r.Box[2] < 55 || r.Box[3] < 55 {
		t.Errorf("motion region %v doesn't cover the expected area [30,30,60,60]", r.Box)
	}
	if r.Area < 100 {
		t.Errorf("expected area >= 100 for 30x30 rect, got %d", r.Area)
	}
}

func TestMotionDetector_TwoSeparateRegions(t *testing.T) {
	md := NewMotionDetector(25, 10, 0.01)
	w, h := 200, 100

	bg := makeRGB(w, h, 50, 50, 50)
	md.Detect(bg, w, h)

	// Two separate bright rectangles far apart
	frame2 := makeRGB(w, h, 50, 50, 50)
	drawRect(frame2, w, 10, 10, 40, 40, 200, 200, 200)   // left side
	drawRect(frame2, w, 150, 60, 190, 90, 200, 200, 200)  // right side

	regions := md.Detect(frame2, w, h)
	if len(regions) != 2 {
		t.Errorf("expected 2 motion regions, got %d", len(regions))
	}
}

func TestMotionDetector_SmallNoiseBelowMinArea(t *testing.T) {
	md := NewMotionDetector(25, 500, 0.01) // high minArea
	w, h := 100, 100

	bg := makeRGB(w, h, 50, 50, 50)
	md.Detect(bg, w, h)

	// Small 5x5 change, area=25, well below minArea=500
	frame2 := makeRGB(w, h, 50, 50, 50)
	drawRect(frame2, w, 45, 45, 50, 50, 200, 200, 200)

	regions := md.Detect(frame2, w, h)
	if len(regions) != 0 {
		t.Errorf("small noise should be filtered, got %d regions", len(regions))
	}
}

func TestMotionDetector_BackgroundAdaptation(t *testing.T) {
	md := NewMotionDetector(25, 10, 0.5) // high alpha for fast adaptation
	w, h := 50, 50

	bg := makeRGB(w, h, 50, 50, 50)
	md.Detect(bg, w, h)

	// Sudden change
	changed := makeRGB(w, h, 200, 200, 200)
	regions := md.Detect(changed, w, h)
	if len(regions) == 0 {
		t.Fatal("should detect motion on first change")
	}

	// After many frames of the new value, background adapts
	for i := 0; i < 20; i++ {
		md.Detect(changed, w, h)
	}

	// Now the "changed" frame IS the background, no motion
	regions = md.Detect(changed, w, h)
	if len(regions) != 0 {
		t.Errorf("after adaptation, should see no motion, got %d regions", len(regions))
	}
}

func TestMotionDetector_InvalidFrame(t *testing.T) {
	md := NewMotionDetector(25, 10, 0.05)
	regions := md.Detect([]byte{1, 2, 3}, 100, 100)
	if regions != nil {
		t.Errorf("invalid frame size should return nil")
	}
}

func TestBoxBlur3x3(t *testing.T) {
	// 3x3 image, all 100 except center is 200
	src := []uint8{
		100, 100, 100,
		100, 200, 100,
		100, 100, 100,
	}
	result := make([]uint8, 9)
	boxBlur3x3(src, result, 3, 3)

	// Center pixel should average all 9 neighbors: (8*100 + 200) / 9 = 111
	center := result[4]
	if center < 110 || center > 112 {
		t.Errorf("expected center ~111, got %d", center)
	}

	// Corner pixel averages 4 neighbors
	corner := result[0]
	expected := (100 + 100 + 100 + 200) / 4
	if corner != uint8(expected) {
		t.Errorf("expected corner %d, got %d", expected, corner)
	}
}

func TestMotionDetector_FrameCoverage(t *testing.T) {
	md := NewMotionDetector(25, 50, 0.5)
	w, h := 100, 100

	frame1 := make([]byte, w*h*3)
	md.Detect(frame1, w, h)

	frame2 := make([]byte, w*h*3)
	for i := 0; i < w*h*3/5; i++ {
		frame2[i] = 255
	}
	md.Detect(frame2, w, h)

	coverage := md.FrameCoverage()
	if coverage < 0.1 || coverage > 0.3 {
		t.Errorf("expected frame coverage ~0.2, got %f", coverage)
	}

	for i := 0; i < 10; i++ {
		md.Detect(frame1, w, h)
	}
	coverage = md.FrameCoverage()
	if coverage > 0.05 {
		t.Errorf("expected near-zero coverage after static frames, got %f", coverage)
	}
}

func BenchmarkMotionDetector_Detect(b *testing.B) {
	w, h := 320, 240
	md := NewMotionDetector(25, 50, 0.05)

	bg := makeRGB(w, h, 50, 50, 50)
	md.Detect(bg, w, h) // initialize background

	frame := makeRGB(w, h, 50, 50, 50)
	drawRect(frame, w, 80, 60, 240, 180, 200, 200, 200)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		md.Detect(frame, w, h)
	}
}
