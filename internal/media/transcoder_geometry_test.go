package media

import (
	"image"
	"testing"
)

// TestScaledFrameMatchesTheGeometryHandedToTheEncoder pins the contract
// between the two halves of the resize path. shouldTranscode decides the
// output dimensions and transcodeFile passes them to the encoder as
// iPicWidth/iPicHeight; scaleYCbCr produces the planes the encoder then reads.
// If the two disagree by even one row, the encoder is told to traverse more
// rows than the plane holds.
func TestScaledFrameMatchesTheGeometryHandedToTheEncoder(t *testing.T) {
	// Every resolution a camera in this codebase has been seen to emit,
	// plus the awkward ratios that make the rounding visible.
	sources := []struct{ w, h int }{
		{2560, 1440}, // Reolink main, 4:3-friendly
		{2560, 1920}, // Tapo C220 2K main
		{2688, 1520}, // common 4MP sensor
		{2304, 1296},
		{1920, 1080},
		{3840, 2160},
		{1600, 1200},
		{2592, 1944},
	}
	const targetW, targetH = 1280, 720

	for _, src := range sources {
		skip, outW, outH := shouldTranscode(src.w, src.h, targetW, targetH)
		if skip {
			continue
		}
		frame := image.NewYCbCr(image.Rect(0, 0, src.w, src.h), image.YCbCrSubsampleRatio420)
		scaled := scaleYCbCr(frame, outW, outH)

		gotW := scaled.Rect.Dx()
		gotH := scaled.Rect.Dy()
		if gotW != outW || gotH != outH {
			t.Errorf("%dx%d source: encoder is told %dx%d but scaleYCbCr produced %dx%d",
				src.w, src.h, outW, outH, gotW, gotH)
		}
		if !encoderInputValid(scaled, outW, outH) {
			t.Errorf("%dx%d source: scaled planes are too small for the %dx%d geometry the encoder is given",
				src.w, src.h, outW, outH)
		}
	}
}

// TestScaledFrameKeepsAspectRatio guards the property the exact-dimension
// contract must not cost: the output still fits inside the target box and
// keeps the source shape to within the even-dimension rounding.
func TestScaledFrameKeepsAspectRatio(t *testing.T) {
	const targetW, targetH = 1280, 720
	for _, src := range []struct{ w, h int }{{2688, 1520}, {2560, 1920}, {3840, 2160}} {
		skip, outW, outH := shouldTranscode(src.w, src.h, targetW, targetH)
		if skip {
			t.Fatalf("%dx%d should transcode to %dx%d", src.w, src.h, targetW, targetH)
		}
		if outW > targetW || outH > targetH {
			t.Errorf("%dx%d scaled to %dx%d, outside the %dx%d target box",
				src.w, src.h, outW, outH, targetW, targetH)
		}
		srcRatio := float64(src.w) / float64(src.h)
		outRatio := float64(outW) / float64(outH)
		// Rounding both dimensions down to even can move the ratio by at
		// most one part in the smaller dimension.
		if tolerance := 2 * srcRatio / float64(outH); outRatio < srcRatio-tolerance || outRatio > srcRatio+tolerance {
			t.Errorf("%dx%d scaled to %dx%d: aspect ratio %.4f differs from source %.4f by more than rounding",
				src.w, src.h, outW, outH, outRatio, srcRatio)
		}
	}
}
