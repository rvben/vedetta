package onnxruntime

import (
	"math"
	"testing"
)

func referenceSiLU(x float32) float32 {
	v := float64(x)
	return float32(v / (1 + math.Exp(-v)))
}

// siluTolerance is the largest error the lookup-table sigmoid may add to
// x*sigmoid(x); the vectorized path is far inside it.
func siluTolerance(x float32) float64 {
	return 2e-4 * math.Max(1, math.Abs(float64(x)))
}

func checkSiLU(t *testing.T, got []float32, in []float32, bias func(i int) float32) {
	t.Helper()
	for i, g := range got {
		x := in[i] + bias(i)
		want := referenceSiLU(x)
		if math.Abs(float64(g)-float64(want)) > siluTolerance(x) {
			t.Fatalf("element %d: silu(%g) = %g, want %g", i, x, g, want)
		}
	}
}

// Sizes straddle the scalar/vector threshold and the vector block length, where
// an off-by-one would leave a tail unactivated or run past the row.
func TestApplySiLU_MatchesReferenceAcrossSizes(t *testing.T) {
	for _, n := range []int{1, 7, 255, 256, 257, 1023, 1024, 1025, 2048, 5000} {
		in := make([]float32, n)
		for i := range in {
			in[i] = float32(i%401)/10 - 20 // -20 .. 20 in steps of 0.1
		}
		got := append([]float32(nil), in...)
		applySiLU(got, nil, 0, 1, n)
		checkSiLU(t, got, in, func(int) float32 { return 0 })
	}
}

// Each row takes its own bias, read from the group's offset into the bias
// vector, and neighbouring rows and bias entries stay untouched.
func TestApplySiLU_AddsPerRowBiasFromOffset(t *testing.T) {
	const rows, cols, offset = 3, 700, 2
	bias := []float32{100, 100, -1.5, 0.25, 3, 100}
	in := make([]float32, rows*cols)
	for i := range in {
		in[i] = float32(i%97)/8 - 6
	}
	got := append([]float32(nil), in...)
	applySiLU(got, bias, offset, rows, cols)
	checkSiLU(t, got, in, func(i int) float32 { return bias[offset+i/cols] })

	if bias[0] != 100 || bias[1] != 100 || bias[5] != 100 || bias[2] != -1.5 {
		t.Fatalf("bias vector was modified: %v", bias)
	}
}

// Only the rows named by the arguments are activated; the slice may be a
// window into a larger output tensor.
func TestApplySiLU_LeavesValuesPastTheRowsAlone(t *testing.T) {
	const n = 1500
	buf := make([]float32, n+8)
	for i := range buf {
		buf[i] = 2
	}
	applySiLU(buf[:n], nil, 0, 1, n)
	for i := n; i < len(buf); i++ {
		if buf[i] != 2 {
			t.Fatalf("value %d past the activated rows changed to %g", i, buf[i])
		}
	}
	if math.Abs(float64(buf[n-1])-float64(referenceSiLU(2))) > siluTolerance(2) {
		t.Fatalf("last activated value = %g, want %g", buf[n-1], referenceSiLU(2))
	}
}

// Far tails must saturate rather than overflow: a large negative input tends to
// zero and a large positive one passes through unchanged.
func TestApplySiLU_TailsAndNaN(t *testing.T) {
	const n = 1200
	nan := float32(math.NaN())
	for _, tc := range []struct {
		x    float32
		want float32
	}{
		{-1000, 0}, {-100, 0}, {-30, 0}, {30, 30}, {100, 100}, {1000, 1000}, {0, 0},
	} {
		buf := make([]float32, n)
		for i := range buf {
			buf[i] = tc.x
		}
		applySiLU(buf, nil, 0, 1, n)
		for i, g := range buf {
			if g != g || math.Abs(float64(g)-float64(tc.want)) > 1e-6*math.Max(1, math.Abs(float64(tc.want))) {
				t.Fatalf("silu(%g)[%d] = %g, want %g", tc.x, i, g, tc.want)
			}
		}
	}

	buf := make([]float32, n)
	buf[600] = nan
	applySiLU(buf, nil, 0, 1, n)
	if buf[600] == buf[600] {
		t.Fatalf("NaN input produced %g, want NaN", buf[600])
	}
	if buf[599] != 0 || buf[601] != 0 {
		t.Fatalf("NaN leaked into neighbours: %g, %g", buf[599], buf[601])
	}
}

// One 80x80 feature map of 64 channels, the shape of an early YOLOv8n layer.
func BenchmarkApplySiLU_64ch_80x80(b *testing.B) {
	const rows, cols = 64, 80 * 80
	src := make([]float32, rows*cols)
	for i := range src {
		src[i] = float32(i%1601)/100 - 8
	}
	bias := make([]float32, rows)
	dst := make([]float32, len(src))

	b.SetBytes(int64(len(src) * 4))
	for b.Loop() {
		copy(dst, src)
		applySiLU(dst, bias, 0, rows, cols)
	}
}
