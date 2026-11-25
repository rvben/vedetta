//go:build darwin

package onnxruntime

/*
#include <Accelerate/Accelerate.h>

// The exponent is computed into a stack block so the activation needs no heap
// scratch and a block stays inside the L1 cache across the four passes.
#define VEDETTA_SILU_BLOCK 1024

// x[i] = x[i] / (1 + exp(-x[i])), which is x * sigmoid(x).
static void vedetta_silu(float *x, int n) {
	float t[VEDETTA_SILU_BLOCK];
	const float one = 1.0f;
	while (n > 0) {
		int m = n < VEDETTA_SILU_BLOCK ? n : VEDETTA_SILU_BLOCK;
		vDSP_vneg(x, 1, t, 1, (vDSP_Length)m);
		vvexpf(t, t, &m);
		vDSP_vsadd(t, 1, &one, t, 1, (vDSP_Length)m);
		vDSP_vdiv(t, 1, x, 1, x, 1, (vDSP_Length)m);
		x += m;
		n -= m;
	}
}

static void vedetta_bias_silu(float *dst, const float *bias, int rows, int cols) {
	for (int r = 0; r < rows; r++) {
		float *row = dst + (size_t)r * (size_t)cols;
		if (bias != NULL) {
			vDSP_vsadd(row, 1, &bias[r], row, 1, (vDSP_Length)cols);
		}
		vedetta_silu(row, cols);
	}
}
*/
import "C"

// siluVectorThreshold is the element count below which the lookup-table loop is
// used, because the cgo call costs more than the activation itself.
const siluVectorThreshold = 256

// applySiLU applies bias + SiLU activation in-place on GEMM output: dst holds
// mPerGroup rows of outSpatial values, and row m gets bias[chanOffset+m] added
// before the activation. A nil bias adds nothing.
//
// Large outputs go through Accelerate's vectorized exp. A convolution output is
// tens of thousands of values, and evaluating the sigmoid per element in Go
// costs more than the matrix multiply that produced them.
func applySiLU(dst []float32, bias []float32, chanOffset, mPerGroup, outSpatial int) {
	n := mPerGroup * outSpatial
	if n < siluVectorThreshold {
		applySiLUGo(dst, bias, chanOffset, mPerGroup, outSpatial)
		return
	}
	var biasPtr *C.float
	if bias != nil {
		biasPtr = (*C.float)(&bias[chanOffset : chanOffset+mPerGroup][0])
	}
	C.vedetta_bias_silu((*C.float)(&dst[:n][0]), biasPtr, C.int(mPerGroup), C.int(outSpatial))
}
