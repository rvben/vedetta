package detect

import (
	"image"
	"testing"
)

// SCRFD sees only the crop, and cropRegion clamps the requested person box to
// the frame, so a person box that starts off-frame produces a crop whose origin
// is not where the box asked to start. The face coordinates are relative to the
// crop, so the crop origin is the offset that maps them back.
func TestFaceInFrameUsesCropOrigin(t *testing.T) {
	frame := image.NewRGBA(image.Rect(0, 0, 100, 100))
	personBox := [4]int{-20, -30, 60, 70}
	crop := cropRegion(frame, personBox)

	if got := crop.Bounds().Min; got != (image.Point{X: 0, Y: 0}) {
		t.Fatalf("crop origin = %v, want (0,0): the premise of this test", got)
	}

	face := scrfdFace{
		box:       [4]int{10, 10, 30, 30},
		landmarks: [5][2]float32{{12, 14}, {20, 14}, {16, 20}, {13, 25}, {19, 25}},
		score:     0.9,
	}

	box, landmarks, ok := faceInFrame(face, crop, frame.Bounds())
	if !ok {
		t.Fatal("face inside the frame was dropped")
	}

	wantBox := [4]int{10, 10, 30, 30}
	if box != wantBox {
		t.Errorf("box = %v, want %v", box, wantBox)
	}
	wantLandmark := [2]float32{12, 14}
	if landmarks[0] != wantLandmark {
		t.Errorf("landmark 0 = %v, want %v", landmarks[0], wantLandmark)
	}
}

// A crop that sits inside the frame offsets by its own origin, which is the
// person box unchanged.
func TestFaceInFrameOffsetsByCropOrigin(t *testing.T) {
	frame := image.NewRGBA(image.Rect(0, 0, 200, 200))
	personBox := [4]int{40, 50, 140, 150}
	crop := cropRegion(frame, personBox)

	face := scrfdFace{
		box:       [4]int{10, 20, 30, 40},
		landmarks: [5][2]float32{{12, 22}, {20, 22}, {16, 28}, {13, 33}, {19, 33}},
		score:     0.9,
	}

	box, landmarks, ok := faceInFrame(face, crop, frame.Bounds())
	if !ok {
		t.Fatal("face inside the frame was dropped")
	}

	wantBox := [4]int{50, 70, 70, 90}
	if box != wantBox {
		t.Errorf("box = %v, want %v", box, wantBox)
	}
	wantLandmark := [2]float32{52, 72}
	if landmarks[0] != wantLandmark {
		t.Errorf("landmark 0 = %v, want %v", landmarks[0], wantLandmark)
	}
}

// SCRFD decodes boxes from anchors, so one can extend past the crop and past
// the frame. The box is stored and used as frame pixel coordinates, so it is
// clamped like any other detection box.
func TestFaceInFrameClampsToFrame(t *testing.T) {
	frame := image.NewRGBA(image.Rect(0, 0, 100, 100))
	personBox := [4]int{60, 60, 100, 100}
	crop := cropRegion(frame, personBox)

	face := scrfdFace{
		box:   [4]int{10, 10, 90, 90},
		score: 0.9,
	}

	box, _, ok := faceInFrame(face, crop, frame.Bounds())
	if !ok {
		t.Fatal("face overlapping the frame was dropped")
	}

	wantBox := [4]int{70, 70, 100, 100}
	if box != wantBox {
		t.Errorf("box = %v, want %v", box, wantBox)
	}
}

// A face box that lands entirely outside the frame has no pixels to align,
// embed or draw, so it is dropped.
func TestFaceInFrameDropsBoxOutsideFrame(t *testing.T) {
	frame := image.NewRGBA(image.Rect(0, 0, 100, 100))
	personBox := [4]int{60, 60, 100, 100}
	crop := cropRegion(frame, personBox)

	face := scrfdFace{
		box:   [4]int{50, 50, 80, 80},
		score: 0.9,
	}

	if _, _, ok := faceInFrame(face, crop, frame.Bounds()); ok {
		t.Fatal("face outside the frame was kept")
	}
}
