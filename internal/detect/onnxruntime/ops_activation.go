package onnxruntime

import (
	"fmt"
	"math"
)

func init() {
	Register("Sigmoid", opSigmoid)
	Register("Relu", opRelu)
	Register("PRelu", opPRelu)
	Register("Softmax", opSoftmax)
}

// sigmoidLUT is a lookup table for sigmoid over [-8, 8] with 4096 entries.
// Using a LUT with linear interpolation is ~10x faster than math.Exp.
var sigmoidLUT [sigmoidLUTSize + 1]float32

const (
	sigmoidLUTSize  = 4096
	sigmoidLUTMin   = float32(-8.0)
	sigmoidLUTMax   = float32(8.0)
	sigmoidLUTRange = sigmoidLUTMax - sigmoidLUTMin
	sigmoidLUTScale = sigmoidLUTSize / sigmoidLUTRange
)

func init() {
	for i := range sigmoidLUTSize + 1 {
		x := float64(sigmoidLUTMin) + float64(i)/float64(sigmoidLUTSize)*float64(sigmoidLUTRange)
		sigmoidLUT[i] = float32(1.0 / (1.0 + math.Exp(-x)))
	}
}

func opSigmoid(inputs []*Tensor, _ *Attributes) ([]*Tensor, error) {
	if len(inputs) < 1 {
		return nil, fmt.Errorf("sigmoid requires 1 input, got %d", len(inputs))
	}
	x := inputs[0]
	out := newTensorUninit(x.Shape)

	for i, v := range x.Data {
		out.Data[i] = fastSigmoid(v)
	}

	return []*Tensor{out}, nil
}

// fastSigmoid computes 1/(1+exp(-x)) using a lookup table with linear interpolation.
func fastSigmoid(x float32) float32 {
	// NaN propagates through sigmoid. A direct NaN check avoids the
	// platform-dependent behavior of int(NaN) further down.
	if x != x {
		return x
	}
	if x <= sigmoidLUTMin {
		return float32(1 / (1 + math.Exp(-float64(x))))
	}
	if x >= sigmoidLUTMax {
		return float32(1 / (1 + math.Exp(-float64(x))))
	}
	pos := (x - sigmoidLUTMin) * sigmoidLUTScale
	idx := int(pos)
	// Clamp idx so that idx+1 stays within the LUT. Float32 rounding can
	// produce pos == sigmoidLUTSize when x is just below sigmoidLUTMax.
	if idx >= sigmoidLUTSize {
		return sigmoidLUT[sigmoidLUTSize]
	}
	frac := pos - float32(idx)
	return sigmoidLUT[idx] + frac*(sigmoidLUT[idx+1]-sigmoidLUT[idx])
}

func opRelu(inputs []*Tensor, _ *Attributes) ([]*Tensor, error) {
	if len(inputs) < 1 {
		return nil, fmt.Errorf("relu requires 1 input, got %d", len(inputs))
	}
	x := inputs[0]
	out := newTensorUninit(x.Shape)

	for i, v := range x.Data {
		if v > 0 {
			out.Data[i] = v
		} else {
			out.Data[i] = 0
		}
	}

	return []*Tensor{out}, nil
}

func opPRelu(inputs []*Tensor, _ *Attributes) ([]*Tensor, error) {
	if len(inputs) < 2 {
		return nil, fmt.Errorf("prelu requires 2 inputs, got %d", len(inputs))
	}
	x, slope := inputs[0], inputs[1]

	// Fast path: same shape
	if shapesEqual(x.Shape, slope.Shape) {
		out := newTensorUninit(x.Shape)
		for i, v := range x.Data {
			if v >= 0 {
				out.Data[i] = v
			} else {
				out.Data[i] = slope.Data[i] * v
			}
		}
		return []*Tensor{out}, nil
	}

	// Fast path: scalar slope
	if len(slope.Shape) == 0 || len(slope.Data) == 1 {
		sv := slope.Data[0]
		out := newTensorUninit(x.Shape)
		for i, v := range x.Data {
			if v >= 0 {
				out.Data[i] = v
			} else {
				out.Data[i] = sv * v
			}
		}
		return []*Tensor{out}, nil
	}

	// Fast path: per-channel broadcast [N,C,H,W] with slope [C,1,1] or [1,C,1,1]
	if spatialSize, channels, ok := isPerChannelBroadcast(x.Shape, slope.Shape); ok {
		out := newTensorUninit(x.Shape)
		n := int(x.Shape[0])
		idx := 0
		for ni := 0; ni < n; ni++ {
			for c := 0; c < channels; c++ {
				sv := slope.Data[c]
				for s := 0; s < spatialSize; s++ {
					v := x.Data[idx]
					if v >= 0 {
						out.Data[idx] = v
					} else {
						out.Data[idx] = sv * v
					}
					idx++
				}
			}
		}
		return []*Tensor{out}, nil
	}

	// Generic broadcast fallback
	outShape, err := broadcastShapes(x.Shape, slope.Shape)
	if err != nil {
		return nil, fmt.Errorf("prelu: %w", err)
	}
	size := int(tensorSize(outShape))
	out := newTensorUninit(outShape)
	for i := 0; i < size; i++ {
		xi := broadcastIndex(int64(i), outShape, x.Shape)
		si := broadcastIndex(int64(i), outShape, slope.Shape)
		v := x.Data[xi]
		if v >= 0 {
			out.Data[i] = v
		} else {
			out.Data[i] = slope.Data[si] * v
		}
	}
	return []*Tensor{out}, nil
}

func opSoftmax(inputs []*Tensor, attrs *Attributes) ([]*Tensor, error) {
	if len(inputs) < 1 {
		return nil, fmt.Errorf("softmax requires 1 input, got %d", len(inputs))
	}
	x := inputs[0]
	axis := int(attrs.GetInt("axis", -1))

	ndim := len(x.Shape)
	if axis < 0 {
		axis += ndim
	}
	if axis < 0 || axis >= ndim {
		return nil, fmt.Errorf("softmax: axis %d out of range for %dD tensor", axis, ndim)
	}

	out := NewTensor(x.Shape, nil)
	copy(out.Data, x.Data)

	axisSize := int(x.Shape[axis])

	// Compute stride for the softmax axis
	innerSize := 1
	for i := axis + 1; i < ndim; i++ {
		innerSize *= int(x.Shape[i])
	}

	outerSize := len(x.Data) / (axisSize * innerSize)

	for outer := 0; outer < outerSize; outer++ {
		for inner := 0; inner < innerSize; inner++ {
			base := outer*axisSize*innerSize + inner

			// Find max for numerical stability
			maxVal := float32(math.Inf(-1))
			for a := 0; a < axisSize; a++ {
				idx := base + a*innerSize
				if out.Data[idx] > maxVal {
					maxVal = out.Data[idx]
				}
			}

			// Exp and sum
			sum := float32(0)
			for a := 0; a < axisSize; a++ {
				idx := base + a*innerSize
				out.Data[idx] = float32(math.Exp(float64(out.Data[idx] - maxVal)))
				sum += out.Data[idx]
			}

			// Normalize
			for a := 0; a < axisSize; a++ {
				idx := base + a*innerSize
				out.Data[idx] /= sum
			}
		}
	}

	return []*Tensor{out}, nil
}
