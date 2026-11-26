package detect

import (
	"math"
	"math/rand/v2"
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

// makeGray creates a uniform frame of one grey level per pixel.
func makeGray(w, h int, level uint8) []byte {
	frame := make([]byte, w*h)
	for i := range frame {
		frame[i] = level
	}
	return frame
}

// drawRect fills a rectangle in a grey frame with the given level.
func drawRect(frame []byte, w, x1, y1, x2, y2 int, level uint8) {
	for y := y1; y < y2; y++ {
		for x := x1; x < x2; x++ {
			frame[y*w+x] = level
		}
	}
}

func TestMotionDetector_StaticScene(t *testing.T) {
	md := NewMotionDetector(25, 50, 0.05)
	w, h := 100, 100
	frame := makeGray(w, h, 128)

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
	bg := makeGray(w, h, 50)
	md.Detect(bg, w, h)

	// Frame 2: bright rectangle appears
	frame2 := makeGray(w, h, 50)
	drawRect(frame2, w, 30, 30, 60, 60, 200)
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

	bg := makeGray(w, h, 50)
	md.Detect(bg, w, h)

	// Two separate bright rectangles far apart
	frame2 := makeGray(w, h, 50)
	drawRect(frame2, w, 10, 10, 40, 40, 200)   // left side
	drawRect(frame2, w, 150, 60, 190, 90, 200) // right side

	regions := md.Detect(frame2, w, h)
	if len(regions) != 2 {
		t.Errorf("expected 2 motion regions, got %d", len(regions))
	}
}

func TestMotionDetector_SmallNoiseBelowMinArea(t *testing.T) {
	md := NewMotionDetector(25, 500, 0.01) // high minArea
	w, h := 100, 100

	bg := makeGray(w, h, 50)
	md.Detect(bg, w, h)

	// Small 5x5 change, area=25, well below minArea=500
	frame2 := makeGray(w, h, 50)
	drawRect(frame2, w, 45, 45, 50, 50, 200)

	regions := md.Detect(frame2, w, h)
	if len(regions) != 0 {
		t.Errorf("small noise should be filtered, got %d regions", len(regions))
	}
}

func TestMotionDetector_BackgroundAdaptation(t *testing.T) {
	md := NewMotionDetector(25, 10, 0.5) // high alpha for fast adaptation
	w, h := 50, 50

	bg := makeGray(w, h, 50)
	md.Detect(bg, w, h)

	// Sudden change
	changed := makeGray(w, h, 200)
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

	frame1 := make([]byte, w*h)
	md.Detect(frame1, w, h)

	frame2 := make([]byte, w*h)
	for i := 0; i < w*h/5; i++ {
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

	bg := makeGray(w, h, 50)
	md.Detect(bg, w, h) // initialize background

	frame := makeGray(w, h, 50)
	drawRect(frame, w, 80, 60, 240, 180, 200)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		md.Detect(frame, w, h)
	}
}

// noisyGray returns frames of a mid-grey scene with a few levels of sensor
// noise, the input a camera gives while nothing moves.
func noisyGray(w, h, frames int) [][]byte {
	rng := rand.New(rand.NewPCG(1, 2))
	out := make([][]byte, frames)
	for f := range out {
		frame := make([]byte, w*h)
		for i := range frame {
			frame[i] = uint8(118 + rng.IntN(5))
		}
		out[f] = frame
	}
	return out
}

func BenchmarkMotionDetector_QuietScene(b *testing.B) {
	w, h := 640, 480
	md := NewMotionDetector(25, 200, 0.05)
	frames := noisyGray(w, h, 8)
	md.Detect(frames[0], w, h)

	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		md.Detect(frames[i%len(frames)], w, h)
	}
}

// TestMotionDetectorBackgroundFollowsTheBlend checks the fixed-point background
// against the blend it stands for, computed in float64: after every frame each
// pixel of the model is within a small fraction of a grey level of
// bg*(1-alpha) + frame*alpha, and a pixel is foreground exactly when it differs
// from the model by more than the threshold, except where that difference is
// too close to the threshold for the two computations to agree.
func TestMotionDetectorBackgroundFollowsTheBlend(t *testing.T) {
	const (
		w, h      = 96, 64
		threshold = 25
		tolerance = 0.05
	)
	for _, alpha := range []float64{0.01, 0.05, 0.5, 1} {
		md := NewMotionDetector(threshold, 10, alpha)
		rng := rand.New(rand.NewPCG(7, uint64(alpha*1000)))
		var want []float64
		foreground := 0
		for f := range 120 {
			// A noisy scene that brightens slowly, crossed by a bright block.
			frame := make([]byte, w*h)
			for i := range frame {
				frame[i] = uint8(60 + f/2 + rng.IntN(7))
			}
			if f > 0 {
				x := (f * 3) % (w - 20)
				drawRect(frame, w, x, 20, x+20, 40, uint8(100+f))
			}
			md.Detect(frame, w, h)

			if want == nil {
				want = make([]float64, w*h)
				for i, v := range frame {
					want[i] = float64(v)
				}
				continue
			}
			for i, v := range md.blurred {
				diff := math.Abs(float64(v) - want[i])
				if math.Abs(diff-threshold) > tolerance {
					if fg := diff > threshold; fg != (md.binary[i] == 1) {
						t.Fatalf("alpha %v frame %d pixel %d: foreground is %v, want %v (difference %.3f)", alpha, f, i, md.binary[i] == 1, fg, diff)
					}
				}
				foreground += int(md.binary[i])
				want[i] = want[i]*(1-alpha) + float64(v)*alpha
				got := float64(md.bg[i]) / (1 << bgFracBits)
				if math.Abs(got-want[i]) > tolerance {
					t.Fatalf("alpha %v frame %d pixel %d: background is %.4f, want %.4f", alpha, f, i, got, want[i])
				}
			}
		}
		if foreground == 0 {
			t.Fatalf("alpha %v: the sequence produced no foreground, so the comparison proved nothing", alpha)
		}
	}
}

func TestNewMotionDetectorKeepsTheBlendingFactorInRange(t *testing.T) {
	for alpha, want := range map[float64]int64{-1: 0, 0: 0, 0.5: 1 << (bgFracBits - 1), 1: 1 << bgFracBits, 3: 1 << bgFracBits} {
		if got := NewMotionDetector(25, 10, alpha).bgAlpha; got != want {
			t.Errorf("blending factor %v is stored as %d, want %d", alpha, got, want)
		}
	}
	if got := NewMotionDetector(25, 10, math.NaN()).bgAlpha; got != 0 {
		t.Errorf("a blending factor that is not a number is stored as %d, want 0", got)
	}
	if got, want := NewMotionDetector(25, 10, math.Inf(1)).bgAlpha, int64(1<<bgFracBits); got != want {
		t.Errorf("an infinite blending factor is stored as %d, want %d", got, want)
	}
}

// A scene that changes and then holds has to become the background exactly.
// A background that stops a fraction short leaves a pixel at the threshold on
// the wrong side of it for as long as the scene holds.
func TestMotionDetectorBackgroundSettlesOnALevelThatHolds(t *testing.T) {
	const (
		w, h      = 32, 32
		threshold = 25
	)
	gray := func(level uint8) []byte { return makeGray(w, h, level) }
	for _, tc := range []struct {
		name                       string
		from, to                   uint8
		atThreshold, pastThreshold uint8
	}{
		{"brighter", 100, 119, 119 + threshold, 119 + threshold + 1},
		{"darker", 119, 100, 100 - threshold, 100 - threshold - 1},
	} {
		settled := func() *MotionDetector {
			md := NewMotionDetector(threshold, 10, 0.05)
			md.Detect(gray(tc.from), w, h)
			for range 600 {
				md.Detect(gray(tc.to), w, h)
			}
			return md
		}

		md := settled()
		for i, level := range md.bg {
			if level != int32(tc.to)<<bgFracBits {
				t.Fatalf("%s: background pixel %d is %.5f after the scene held at %d", tc.name, i, float64(level)/(1<<bgFracBits), tc.to)
			}
		}
		md.Detect(gray(tc.atThreshold), w, h)
		if got := md.FrameCoverage(); got != 0 {
			t.Fatalf("%s: a change of exactly the threshold marked %.0f%% of the frame, want none", tc.name, got*100)
		}

		md = settled()
		md.Detect(gray(tc.pastThreshold), w, h)
		if got := md.FrameCoverage(); got != 1 {
			t.Fatalf("%s: a change of one level past the threshold marked %.0f%% of the frame, want all of it", tc.name, got*100)
		}
	}
}

// A positive blending factor below the detector's resolution still adapts.
func TestMotionDetectorAdaptsWithAVerySmallBlendingFactor(t *testing.T) {
	const w, h = 32, 32
	md := NewMotionDetector(25, 10, 1e-9)
	md.Detect(makeGray(w, h, 100), w, h)
	md.Detect(makeGray(w, h, 140), w, h)
	if got, start := md.bg[0], int32(100)<<bgFracBits; got <= start {
		t.Fatalf("the background did not move towards a brighter scene: %d, started at %d", got, start)
	}

	static := NewMotionDetector(25, 10, 0)
	static.Detect(makeGray(w, h, 100), w, h)
	static.Detect(makeGray(w, h, 140), w, h)
	if got, start := static.bg[0], int32(100)<<bgFracBits; got != start {
		t.Fatalf("a blending factor of 0 moved the background: %d, started at %d", got, start)
	}
}

// A caller that hands over anything but one grey level per pixel, such as a
// packed colour frame, gets no regions and leaves the background alone.
func TestMotionDetectorRejectsAFrameOfTheWrongLength(t *testing.T) {
	const w, h = 64, 48
	md := NewMotionDetector(25, 20, 0.05)
	md.Detect(makeGray(w, h, 0), w, h)

	for _, size := range []int{w*h - 1, w*h + 1, w * h * 3, w*h + w*h/2} {
		if regions := md.Detect(make([]byte, size), w, h); regions != nil {
			t.Fatalf("a %d byte frame for %dx%d gave %d regions, want none", size, w, h, len(regions))
		}
	}

	moved := makeGray(w, h, 0)
	drawRect(moved, w, 10, 10, 30, 30, 255)
	if regions := md.Detect(moved, w, h); len(regions) != 1 {
		t.Fatalf("after the rejected frames a moving block gave %d regions, want 1", len(regions))
	}
}
