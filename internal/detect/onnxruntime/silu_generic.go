//go:build !darwin

package onnxruntime

// applySiLU applies bias + SiLU activation in-place on GEMM output: dst holds
// mPerGroup rows of outSpatial values, and row m gets bias[chanOffset+m] added
// before the activation. A nil bias adds nothing.
func applySiLU(dst []float32, bias []float32, chanOffset, mPerGroup, outSpatial int) {
	applySiLUGo(dst, bias, chanOffset, mPerGroup, outSpatial)
}
