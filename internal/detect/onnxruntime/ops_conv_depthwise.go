package onnxruntime

// convDepthwise computes a convolution in which every group has one input and
// one output channel, writing all N*C output planes into out.
//
// Such a convolution is a separate small filter per channel, so the general
// route (copy every kernel window into a column buffer, then multiply by a
// one-row weight matrix) spends most of its time copying. Here each kernel tap
// is instead added straight into the output plane a row at a time: the taps
// that fall in the padding are skipped by clamping the output range per tap, so
// the inner loop has no bounds or padding test.
func convDepthwise(x, w, out []float32, n, c, h, wd, kH, kW, strideH, strideW, padTop, padLeft, dilH, dilW, outH, outW int) {
	outSpatial := outH * outW
	for plane := range n * c {
		ch := plane % c
		src := x[plane*h*wd : (plane+1)*h*wd]
		dst := out[plane*outSpatial : (plane+1)*outSpatial]
		clear(dst)
		kernel := w[ch*kH*kW : (ch+1)*kH*kW]
		for ky := range kH {
			// Output rows whose input row for this tap lies inside the image.
			offY := ky*dilH - padTop
			oyLo, oyHi := tapRange(offY, strideH, h, outH)
			for kx := range kW {
				wv := kernel[ky*kW+kx]
				offX := kx*dilW - padLeft
				oxLo, oxHi := tapRange(offX, strideW, wd, outW)
				if oxLo >= oxHi {
					continue
				}
				for oy := oyLo; oy < oyHi; oy++ {
					srcRow := src[(oy*strideH+offY)*wd:][:wd]
					dstRow := dst[oy*outW+oxLo : oy*outW+oxHi]
					if strideW == 1 {
						in := srcRow[oxLo+offX:][:len(dstRow)]
						for i, v := range in {
							dstRow[i] += wv * v
						}
						continue
					}
					ix := oxLo*strideW + offX
					for i := range dstRow {
						dstRow[i] += wv * srcRow[ix]
						ix += strideW
					}
				}
			}
		}
	}
}

// tapRange returns the half-open range of output positions o, within
// [0, outSize), for which the input position o*stride+off lies in [0, size).
func tapRange(off, stride, size, outSize int) (lo, hi int) {
	if off < 0 {
		// The ceiling of -off/stride, written so that a huge stride cannot overflow.
		lo = (-off-1)/stride + 1
	}
	hi = outSize
	if last := size - 1 - off; last < 0 {
		hi = 0
	} else if last/stride+1 < hi {
		hi = last/stride + 1
	}
	return lo, hi
}
