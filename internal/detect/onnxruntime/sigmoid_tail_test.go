package onnxruntime

import (
	"math"
	"testing"
)

func TestSigmoidTailsPreserveSmallScores(t *testing.T) {
	for _, x := range []float32{-80, -20, -12, -8, 8, 12, 20, 80} {
		want := float32(1 / (1 + math.Exp(-float64(x))))
		if got := fastSigmoid(x); got != want {
			t.Fatalf("sigmoid(%g)=%g want %g", x, got, want)
		}
	}
}
