package onnxruntime

import (
	"strings"
	"testing"
)

// resizeAttrs builds the attribute set for a Resize node.
func resizeAttrs(mode, coordMode, nearestMode string) *Attributes {
	attrs := NewAttributes()
	attrs.Strings["mode"] = mode
	if coordMode != "" {
		attrs.Strings["coordinate_transformation_mode"] = coordMode
	}
	if nearestMode != "" {
		attrs.Strings["nearest_mode"] = nearestMode
	}
	return attrs
}

// resizeWidth resizes a 1 by 1 by 1 by len(data) tensor to outW columns.
func resizeWidth(t *testing.T, data []float32, outW int, attrs *Attributes) []float32 {
	t.Helper()

	x := NewTensor([]int64{1, 1, 1, int64(len(data))}, data)
	sizes := NewTensor([]int64{4}, []float32{1, 1, 1, float32(outW)})

	out, err := opResize([]*Tensor{x, nil, nil, sizes}, attrs)
	if err != nil {
		t.Fatalf("resize: %v", err)
	}
	return out[0].Data
}

func assertFloats(t *testing.T, got, want []float32, tol float32) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("length = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		d := got[i] - want[i]
		if d < 0 {
			d = -d
		}
		if d > tol {
			t.Fatalf("values = %v, want %v", got, want)
		}
	}
}

// ONNX defaults coordinate_transformation_mode to half_pixel. Reading the
// attribute has to start with honouring its default, which differs from
// asymmetric wherever the sample points do not line up.
func TestResizeNearestDefaultsToHalfPixel(t *testing.T) {
	got := resizeWidth(t, []float32{1, 2, 3, 4, 5}, 2, resizeAttrs("nearest", "", ""))
	assertFloats(t, got, []float32{2, 4}, 0)
}

// The bundled YOLOv8 model declares asymmetric with floor, and its sampling
// must not move.
func TestResizeNearestAsymmetric(t *testing.T) {
	got := resizeWidth(t, []float32{1, 2, 3, 4, 5}, 2, resizeAttrs("nearest", "asymmetric", "floor"))
	assertFloats(t, got, []float32{1, 3}, 0)
}

func TestResizeNearestAlignCorners(t *testing.T) {
	got := resizeWidth(t, []float32{1, 2, 3, 4, 5}, 2, resizeAttrs("nearest", "align_corners", "floor"))
	assertFloats(t, got, []float32{1, 5}, 0)
}

// pytorch_half_pixel differs from half_pixel only when the output has a single
// element, where it samples the first input element instead.
func TestResizeNearestPytorchHalfPixelSingleOutput(t *testing.T) {
	got := resizeWidth(t, []float32{1, 2, 3, 4}, 1, resizeAttrs("nearest", "pytorch_half_pixel", "floor"))
	assertFloats(t, got, []float32{1}, 0)

	got = resizeWidth(t, []float32{1, 2, 3, 4}, 1, resizeAttrs("nearest", "half_pixel", "floor"))
	assertFloats(t, got, []float32{2}, 0)
}

// nearest_mode decides which side a coordinate landing exactly halfway takes.
func TestResizeNearestModeRounding(t *testing.T) {
	data := []float32{1, 2}

	tests := []struct {
		nearestMode string
		want        []float32
	}{
		{"floor", []float32{1, 1, 2, 2}},
		{"ceil", []float32{1, 2, 2, 2}},
		{"round_prefer_floor", []float32{1, 1, 2, 2}},
		{"round_prefer_ceil", []float32{1, 2, 2, 2}},
	}

	for _, tt := range tests {
		t.Run(tt.nearestMode, func(t *testing.T) {
			got := resizeWidth(t, data, 4, resizeAttrs("nearest", "asymmetric", tt.nearestMode))
			assertFloats(t, got, tt.want, 0)
		})
	}
}

func TestResizeLinearAlignCorners(t *testing.T) {
	got := resizeWidth(t, []float32{1, 2, 3}, 5, resizeAttrs("linear", "align_corners", ""))
	assertFloats(t, got, []float32{1, 1.5, 2, 2.5, 3}, 1e-5)
}

func TestResizeLinearAsymmetric(t *testing.T) {
	got := resizeWidth(t, []float32{1, 2, 3}, 6, resizeAttrs("linear", "asymmetric", ""))
	assertFloats(t, got, []float32{1, 1.5, 2, 2.5, 3, 3}, 1e-5)
}

// A transformation this engine does not implement has to be reported. Ignoring
// it samples the wrong pixels and silently degrades detection instead.
func TestResizeUnsupportedCoordinateTransformation(t *testing.T) {
	x := NewTensor([]int64{1, 1, 1, 4}, []float32{1, 2, 3, 4})
	sizes := NewTensor([]int64{4}, []float32{1, 1, 1, 2})

	_, err := opResize([]*Tensor{x, nil, nil, sizes}, resizeAttrs("nearest", "tf_crop_and_resize", ""))
	if err == nil {
		t.Fatal("expected an error for tf_crop_and_resize")
	}
	if !strings.Contains(err.Error(), "tf_crop_and_resize") {
		t.Errorf("error %q does not name the mode", err)
	}

	_, err = opResize([]*Tensor{x, nil, nil, sizes}, resizeAttrs("nearest", "made_up_mode", ""))
	if err == nil {
		t.Fatal("expected an error for an unknown coordinate_transformation_mode")
	}
}

// The bundled YOLOv8 model declares asymmetric with floor on both of its
// Resize nodes, so that combination decides every detection this NVR makes.
// Sampling it through a coordinate transformation has to land on exactly the
// index the integer ratio picks, for every shape.
func TestResizeNearestAsymmetricMatchesIntegerRatio(t *testing.T) {
	shapes := []struct{ in, out int64 }{
		{20, 40}, // the bundled model's first Resize
		{40, 80}, // the bundled model's second Resize
		{1, 7},
		{3, 7},
		{7, 3},
		{5, 5},
		{9, 2},
		{49, 7},
		{7, 49},
		{13, 31},
		{31, 13},
	}

	for _, s := range shapes {
		data := make([]float32, s.in)
		for i := range data {
			data[i] = float32(i)
		}

		got := resizeWidth(t, data, int(s.out), resizeAttrs("nearest", "asymmetric", "floor"))

		want := make([]float32, s.out)
		for i := range want {
			want[i] = data[int64(i)*s.in/s.out]
		}

		for i := range want {
			if got[i] != want[i] {
				t.Errorf("resize %d to %d: index %d sampled %v, want %v",
					s.in, s.out, i, got[i], want[i])
			}
		}
	}
}

func TestResizeUnsupportedNearestMode(t *testing.T) {
	x := NewTensor([]int64{1, 1, 1, 4}, []float32{1, 2, 3, 4})
	sizes := NewTensor([]int64{4}, []float32{1, 1, 1, 2})

	_, err := opResize([]*Tensor{x, nil, nil, sizes}, resizeAttrs("nearest", "asymmetric", "made_up_mode"))
	if err == nil {
		t.Fatal("expected an error for an unknown nearest_mode")
	}
	if !strings.Contains(err.Error(), "made_up_mode") {
		t.Errorf("error %q does not name the mode", err)
	}
}
