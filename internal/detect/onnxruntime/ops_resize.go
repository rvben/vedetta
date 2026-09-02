package onnxruntime

import (
	"fmt"
	"math"
)

func init() {
	Register("Resize", opResize)
}

// coordTransform maps an output index on one axis to a coordinate in the input,
// following the ONNX coordinate_transformation_mode attribute.
type coordTransform func(outIdx int64) float64

// nearestRound turns an input coordinate into the sample index that the ONNX
// nearest_mode attribute selects.
type nearestRound func(coord float64) float64

func opResize(inputs []*Tensor, attrs *Attributes) ([]*Tensor, error) {
	if len(inputs) < 1 {
		return nil, fmt.Errorf("resize: need at least 1 input")
	}
	x := inputs[0]
	if len(x.Shape) != 4 {
		return nil, fmt.Errorf("resize: expected 4D input [N,C,H,W], got %dD", len(x.Shape))
	}

	n := x.Shape[0]
	c := x.Shape[1]
	inH := x.Shape[2]
	inW := x.Shape[3]
	if inH <= 0 || inW <= 0 {
		return nil, fmt.Errorf("resize: invalid input size %dx%d", inH, inW)
	}

	var outH, outW int64
	var scaleH, scaleW float64

	// The output size comes from scales (input[2]) or sizes (input[3]). The
	// coordinate transformations are defined in terms of the ratio each axis is
	// resized by, so keep it rather than recomputing it per axis.
	switch {
	case len(inputs) > 3 && inputs[3] != nil && len(inputs[3].Data) == 4:
		// sizes tensor: [N, C, outH, outW]
		outH = int64(inputs[3].Data[2])
		outW = int64(inputs[3].Data[3])
		scaleH = float64(outH) / float64(inH)
		scaleW = float64(outW) / float64(inW)
	case len(inputs) > 2 && inputs[2] != nil && len(inputs[2].Data) == 4:
		// scales tensor: [scaleN, scaleC, scaleH, scaleW]
		scaleH = float64(inputs[2].Data[2])
		scaleW = float64(inputs[2].Data[3])
		outH = int64(float32(inH) * inputs[2].Data[2])
		outW = int64(float32(inW) * inputs[2].Data[3])
	default:
		return nil, fmt.Errorf("resize: need either scales or sizes input")
	}

	if outH <= 0 || outW <= 0 {
		return nil, fmt.Errorf("resize: invalid output size %dx%d", outH, outW)
	}

	mode := attrs.GetString("mode", "nearest")
	if mode != "nearest" && mode != "linear" {
		return nil, fmt.Errorf("resize: unsupported mode %q", mode)
	}

	coordMode := attrs.GetString("coordinate_transformation_mode", "half_pixel")
	transformH, err := newCoordTransform(coordMode, inH, outH, scaleH)
	if err != nil {
		return nil, err
	}
	transformW, err := newCoordTransform(coordMode, inW, outW, scaleW)
	if err != nil {
		return nil, err
	}

	out := NewTensor([]int64{n, c, outH, outW}, nil)

	if mode == "nearest" {
		round, err := newNearestRound(attrs.GetString("nearest_mode", "round_prefer_floor"))
		if err != nil {
			return nil, err
		}
		resizeNearest(x, out, n, c, inH, inW, outH, outW,
			nearestIndices(transformH, round, inH, outH),
			nearestIndices(transformW, round, inW, outW))
		return []*Tensor{out}, nil
	}

	loY, hiY, fracY := linearTaps(transformH, inH, outH)
	loX, hiX, fracX := linearTaps(transformW, inW, outW)
	resizeBilinear(x, out, n, c, inH, inW, outH, outW, loY, hiY, fracY, loX, hiX, fracX)
	return []*Tensor{out}, nil
}

// newCoordTransform builds the output-to-input coordinate mapping for one axis.
// scale is the ratio that axis is resized by, and inLen and outLen are its
// lengths. An unimplemented mode is an error: sampling the wrong pixels
// produces a plausible image and silently degrades whatever reads it.
func newCoordTransform(mode string, inLen, outLen int64, scale float64) (coordTransform, error) {
	halfPixel := func(i int64) float64 { return (float64(i)+0.5)/scale - 0.5 }

	switch mode {
	case "half_pixel":
		return halfPixel, nil

	case "pytorch_half_pixel":
		// A single output sample has no pixel centre to align to, so it reads
		// the start of the input.
		if outLen <= 1 {
			return func(int64) float64 { return 0 }, nil
		}
		return halfPixel, nil

	case "align_corners":
		if outLen <= 1 {
			return func(int64) float64 { return 0 }, nil
		}
		ratio := float64(inLen-1) / float64(outLen-1)
		return func(i int64) float64 { return float64(i) * ratio }, nil

	case "asymmetric":
		return func(i int64) float64 { return float64(i) / scale }, nil

	case "half_pixel_symmetric":
		// An integer output length rounds the scale, which shifts the sampled
		// window off centre. The offset puts it back.
		adjustment := float64(outLen) / (scale * float64(inLen))
		offset := float64(inLen) / 2 * (1 - adjustment)
		return func(i int64) float64 { return offset + (float64(i)+0.5)/scale - 0.5 }, nil

	case "tf_crop_and_resize":
		return nil, fmt.Errorf("resize: coordinate_transformation_mode %q needs the roi input, which this engine does not read", mode)

	default:
		return nil, fmt.Errorf("resize: unsupported coordinate_transformation_mode %q", mode)
	}
}

// newNearestRound builds the rounding rule that decides which side a coordinate
// landing between two input samples takes.
func newNearestRound(mode string) (nearestRound, error) {
	switch mode {
	case "round_prefer_floor":
		// Exactly halfway takes the lower sample.
		return func(v float64) float64 { return math.Ceil(v - 0.5) }, nil
	case "round_prefer_ceil":
		// Exactly halfway takes the higher sample.
		return func(v float64) float64 { return math.Floor(v + 0.5) }, nil
	case "floor":
		return math.Floor, nil
	case "ceil":
		return math.Ceil, nil
	default:
		return nil, fmt.Errorf("resize: unsupported nearest_mode %q", mode)
	}
}

// nearestIndices precomputes, for one axis, the input index each output index
// reads from, so the sampling loop is a table lookup.
func nearestIndices(transform coordTransform, round nearestRound, inLen, outLen int64) []int64 {
	idx := make([]int64, outLen)
	for i := range idx {
		idx[i] = clampIndex(int64(round(transform(int64(i)))), inLen)
	}
	return idx
}

// linearTaps precomputes, for one axis, the two input samples each output index
// blends and the weight of the second one. The coordinate is clamped to the
// input before its fractional part is taken, so an output falling outside the
// input reads the edge sample rather than a blend with it.
func linearTaps(transform coordTransform, inLen, outLen int64) (lo, hi []int64, frac []float32) {
	lo = make([]int64, outLen)
	hi = make([]int64, outLen)
	frac = make([]float32, outLen)

	limit := float64(inLen - 1)
	for i := range lo {
		v := transform(int64(i))
		if v < 0 {
			v = 0
		}
		if v > limit {
			v = limit
		}
		base := math.Floor(v)
		lo[i] = int64(base)
		hi[i] = clampIndex(lo[i]+1, inLen)
		frac[i] = float32(v - base)
	}
	return lo, hi, frac
}

func clampIndex(i, length int64) int64 {
	if i < 0 {
		return 0
	}
	if i >= length {
		return length - 1
	}
	return i
}

func resizeNearest(x, out *Tensor, n, c, inH, inW, outH, outW int64, srcY, srcX []int64) {
	for ni := int64(0); ni < n; ni++ {
		for ci := int64(0); ci < c; ci++ {
			inBase := (ni*c + ci) * inH * inW
			outBase := (ni*c + ci) * outH * outW
			for y := int64(0); y < outH; y++ {
				row := inBase + srcY[y]*inW
				dst := out.Data[outBase+y*outW : outBase+(y+1)*outW]
				for xi := range dst {
					dst[xi] = x.Data[row+srcX[xi]]
				}
			}
		}
	}
}

func resizeBilinear(x, out *Tensor, n, c, inH, inW, outH, outW int64,
	loY, hiY []int64, fracY []float32, loX, hiX []int64, fracX []float32,
) {
	for ni := int64(0); ni < n; ni++ {
		for ci := int64(0); ci < c; ci++ {
			inBase := (ni*c + ci) * inH * inW
			outBase := (ni*c + ci) * outH * outW
			for y := int64(0); y < outH; y++ {
				row0 := inBase + loY[y]*inW
				row1 := inBase + hiY[y]*inW
				fy := fracY[y]
				dst := out.Data[outBase+y*outW : outBase+(y+1)*outW]
				for xi := range dst {
					x0, x1, fx := loX[xi], hiX[xi], fracX[xi]
					top := x.Data[row0+x0] + (x.Data[row0+x1]-x.Data[row0+x0])*fx
					bottom := x.Data[row1+x0] + (x.Data[row1+x1]-x.Data[row1+x0])*fx
					dst[xi] = top + (bottom-top)*fy
				}
			}
		}
	}
}
