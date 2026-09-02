package detect

import "testing"

// yoloOutput builds a raw YOLOv8 output tensor holding a single detection at
// candidate index i, with the given class score and centre-format box.
func yoloOutput(i, class int, score float32, cx, cy, w, h float32) []float32 {
	out := make([]float32, (4+numClasses)*numDetections)
	out[0*numDetections+i] = cx
	out[1*numDetections+i] = cy
	out[2*numDetections+i] = w
	out[3*numDetections+i] = h
	out[(4+class)*numDetections+i] = score
	return out
}

// A model can place a box partly outside the frame. Every consumer of a
// Detection indexes pixels with it, so it must be clamped where it is built.
func TestProcessOutputClampsBoxToFrame(t *testing.T) {
	const frameW, frameH = 640, 480

	// Centre (10,10) with a 100x100 box reaches (-40,-40)-(60,60).
	out := yoloOutput(0, 0, 0.9, 10, 10, 100, 100)

	dets := processOutput(out, 0.5, 1, 0, 0, frameW, frameH)
	if len(dets) != 1 {
		t.Fatalf("detections = %d, want 1", len(dets))
	}

	want := [4]int{0, 0, 60, 60}
	if dets[0].Box != want {
		t.Errorf("box = %v, want %v", dets[0].Box, want)
	}
}

// A box that clamps to nothing is not a detection anyone can use: it crops an
// empty image and draws no overlay, so it is dropped rather than reported.
func TestProcessOutputDropsBoxOutsideFrame(t *testing.T) {
	const frameW, frameH = 640, 480

	out := yoloOutput(0, 0, 0.9, 1000, 1000, 10, 10)

	dets := processOutput(out, 0.5, 1, 0, 0, frameW, frameH)
	if len(dets) != 0 {
		t.Fatalf("detections = %d, want 0: %v", len(dets), dets)
	}
}

// A box already inside the frame is untouched.
func TestProcessOutputKeepsInteriorBox(t *testing.T) {
	const frameW, frameH = 640, 480

	out := yoloOutput(0, 0, 0.9, 100, 100, 40, 60)

	dets := processOutput(out, 0.5, 1, 0, 0, frameW, frameH)
	if len(dets) != 1 {
		t.Fatalf("detections = %d, want 1", len(dets))
	}
	want := [4]int{80, 70, 120, 130}
	if dets[0].Box != want {
		t.Errorf("box = %v, want %v", dets[0].Box, want)
	}
}

// The decoder is written for the YOLOv8 (1,84,8400) layout. Any other output
// size means the model is not the one this code decodes, which must be said
// rather than read past the end of the tensor.
func TestProcessOutputRejectsWrongOutputSize(t *testing.T) {
	short := make([]float32, 100)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("processOutput panicked on a short output: %v", r)
		}
	}()

	dets := processOutput(short, 0.5, 1, 0, 0, 640, 480)
	if dets != nil {
		t.Fatalf("detections = %v, want none", dets)
	}
}

func TestClampBox(t *testing.T) {
	tests := []struct {
		name     string
		box      [4]int
		w, h     int
		want     [4]int
		wantKeep bool
	}{
		{"inside", [4]int{10, 20, 30, 40}, 100, 100, [4]int{10, 20, 30, 40}, true},
		{"negative origin", [4]int{-5, -8, 30, 40}, 100, 100, [4]int{0, 0, 30, 40}, true},
		{"past far edge", [4]int{10, 20, 300, 400}, 100, 100, [4]int{10, 20, 100, 100}, true},
		{"exactly the frame", [4]int{0, 0, 100, 100}, 100, 100, [4]int{0, 0, 100, 100}, true},
		{"fully left of frame", [4]int{-40, 10, -5, 40}, 100, 100, [4]int{0, 10, 0, 40}, false},
		{"fully below frame", [4]int{10, 200, 40, 300}, 100, 100, [4]int{10, 100, 40, 100}, false},
		{"zero width after clamp", [4]int{100, 10, 120, 40}, 100, 100, [4]int{100, 10, 100, 40}, false},
		{"inverted input", [4]int{50, 50, 20, 20}, 100, 100, [4]int{50, 50, 20, 20}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, keep := clampBox(tt.box, tt.w, tt.h)
			if got != tt.want {
				t.Errorf("box = %v, want %v", got, tt.want)
			}
			if keep != tt.wantKeep {
				t.Errorf("keep = %v, want %v", keep, tt.wantKeep)
			}
		})
	}
}
