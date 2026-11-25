package onnxruntime

import (
	"fmt"
	"math"
	"testing"
)

// referenceConv computes a grouped convolution straight from its definition,
// one output value at a time, as the oracle for the optimised paths.
func referenceConv(x, w, bias []float32, c, h, wd, m, group, kH, kW, strideH, strideW, padTop, padLeft, dilH, dilW, outH, outW int) []float32 {
	cPerGroup, mPerGroup := c/group, m/group
	out := make([]float32, m*outH*outW)
	for om := range m {
		g := om / mPerGroup
		for oy := range outH {
			for ox := range outW {
				var sum float64
				for ic := range cPerGroup {
					for ky := range kH {
						for kx := range kW {
							iy := oy*strideH - padTop + ky*dilH
							ix := ox*strideW - padLeft + kx*dilW
							if iy < 0 || iy >= h || ix < 0 || ix >= wd {
								continue
							}
							sum += float64(w[((om*cPerGroup+ic)*kH+ky)*kW+kx]) * float64(x[((g*cPerGroup+ic)*h+iy)*wd+ix])
						}
					}
				}
				if bias != nil {
					sum += float64(bias[om])
				}
				out[(om*outH+oy)*outW+ox] = float32(sum)
			}
		}
	}
	return out
}

func patternData(n int, seed uint32) []float32 {
	data := make([]float32, n)
	state := seed
	for i := range data {
		state = state*1664525 + 1013904223
		data[i] = float32(state>>8)/float32(1<<23) - 1
	}
	return data
}

// A depthwise convolution (one input and one output channel per group) takes
// its own path through opConv. Every stride, padding and dilation it handles
// must give what the definition gives, including where the kernel hangs over
// an edge on one side only.
func TestConvDepthwiseMatchesDefinition(t *testing.T) {
	cases := []struct {
		h, w, kH, kW, strideH, strideW       int
		padTop, padLeft, padBottom, padRight int
		dilH, dilW                           int
		bias                                 bool
	}{
		{h: 9, w: 7, kH: 3, kW: 3, strideH: 1, strideW: 1, padTop: 1, padLeft: 1, padBottom: 1, padRight: 1, dilH: 1, dilW: 1, bias: true},
		{h: 9, w: 7, kH: 3, kW: 3, strideH: 1, strideW: 1, dilH: 1, dilW: 1},
		{h: 12, w: 10, kH: 3, kW: 3, strideH: 2, strideW: 2, padTop: 1, padLeft: 1, padBottom: 1, padRight: 1, dilH: 1, dilW: 1, bias: true},
		{h: 12, w: 10, kH: 3, kW: 3, strideH: 2, strideW: 2, padBottom: 1, padRight: 1, dilH: 1, dilW: 1},
		{h: 11, w: 13, kH: 5, kW: 3, strideH: 1, strideW: 2, padTop: 2, padLeft: 0, padBottom: 1, padRight: 2, dilH: 1, dilW: 1},
		{h: 11, w: 13, kH: 3, kW: 3, strideH: 1, strideW: 1, padTop: 2, padLeft: 2, padBottom: 2, padRight: 2, dilH: 2, dilW: 2, bias: true},
		{h: 3, w: 3, kH: 3, kW: 3, strideH: 1, strideW: 1, padTop: 2, padLeft: 2, padBottom: 2, padRight: 2, dilH: 1, dilW: 1},
		{h: 40, w: 40, kH: 3, kW: 3, strideH: 1, strideW: 1, padTop: 1, padLeft: 1, padBottom: 1, padRight: 1, dilH: 1, dilW: 1, bias: true},
	}
	const channels = 5
	for i, tc := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			x := NewTensor([]int64{1, channels, int64(tc.h), int64(tc.w)}, patternData(channels*tc.h*tc.w, uint32(i+1)))
			w := NewTensor([]int64{channels, 1, int64(tc.kH), int64(tc.kW)}, patternData(channels*tc.kH*tc.kW, uint32(i+100)))
			inputs := []*Tensor{x, w}
			var bias []float32
			if tc.bias {
				bias = patternData(channels, uint32(i+200))
				inputs = append(inputs, NewTensor([]int64{channels}, bias))
			}
			attrs := NewAttributes()
			attrs.Ints["group"] = channels
			attrs.IntLists["kernel_shape"] = []int64{int64(tc.kH), int64(tc.kW)}
			attrs.IntLists["strides"] = []int64{int64(tc.strideH), int64(tc.strideW)}
			attrs.IntLists["pads"] = []int64{int64(tc.padTop), int64(tc.padLeft), int64(tc.padBottom), int64(tc.padRight)}
			attrs.IntLists["dilations"] = []int64{int64(tc.dilH), int64(tc.dilW)}

			out, err := Execute("Conv", inputs, attrs)
			if err != nil {
				t.Fatal(err)
			}
			outH, outW := int(out[0].Shape[2]), int(out[0].Shape[3])
			want := referenceConv(x.Data, w.Data, bias, channels, tc.h, tc.w, channels, channels,
				tc.kH, tc.kW, tc.strideH, tc.strideW, tc.padTop, tc.padLeft, tc.dilH, tc.dilW, outH, outW)
			if len(out[0].Data) != len(want) {
				t.Fatalf("output has %d values, want %d", len(out[0].Data), len(want))
			}
			var energy float64
			for j := range want {
				energy += math.Abs(float64(want[j]))
				if !(math.Abs(float64(out[0].Data[j])-float64(want[j])) <= 1e-5) {
					t.Fatalf("value %d is %g, want %g", j, out[0].Data[j], want[j])
				}
			}
			if energy == 0 {
				t.Fatal("reference output is all zeros, so agreeing with it proves nothing")
			}
		})
	}
}

// The depthwise path must leave a grouped convolution with more than one
// channel per group to the general path.
func TestConvGroupedNonDepthwiseMatchesDefinition(t *testing.T) {
	const c, m, group, h, wd = 6, 6, 3, 8, 8
	x := NewTensor([]int64{1, c, h, wd}, patternData(c*h*wd, 7))
	w := NewTensor([]int64{m, c / group, 3, 3}, patternData(m*(c/group)*9, 8))
	attrs := NewAttributes()
	attrs.Ints["group"] = group
	attrs.IntLists["kernel_shape"] = []int64{3, 3}
	attrs.IntLists["pads"] = []int64{1, 1, 1, 1}
	out, err := Execute("Conv", []*Tensor{x, w}, attrs)
	if err != nil {
		t.Fatal(err)
	}
	want := referenceConv(x.Data, w.Data, nil, c, h, wd, m, group, 3, 3, 1, 1, 1, 1, 1, 1, h, wd)
	for j := range want {
		if !(math.Abs(float64(out[0].Data[j])-float64(want[j])) <= 1e-5) {
			t.Fatalf("value %d is %g, want %g", j, out[0].Data[j], want[j])
		}
	}
}

func BenchmarkConvDepthwise3x3_128ch_24x16(b *testing.B) {
	x := NewTensor([]int64{1, 128, 24, 16}, patternData(128*24*16, 1))
	w := NewTensor([]int64{128, 1, 3, 3}, patternData(128*9, 2))
	attrs := NewAttributes()
	attrs.Ints["group"] = 128
	attrs.IntLists["kernel_shape"] = []int64{3, 3}
	attrs.IntLists["pads"] = []int64{1, 1, 1, 1}
	inputs := []*Tensor{x, w}
	for b.Loop() {
		_, _ = Execute("Conv", inputs, attrs)
	}
}

func BenchmarkRelu_128ch_48x32(b *testing.B) {
	x := NewTensor([]int64{1, 128, 48, 32}, patternData(128*48*32, 3))
	inputs := []*Tensor{x}
	attrs := NewAttributes()
	for b.Loop() {
		_, _ = Execute("Relu", inputs, attrs)
	}
}

// tapRange bounds the inner loops, so a range that is too wide reads outside
// the input. Each case is checked against the definition by trying every
// output position.
func TestTapRange(t *testing.T) {
	cases := []struct{ off, stride, size, outSize int }{
		{0, 1, 5, 5}, {-1, 1, 5, 5}, {1, 1, 5, 5}, {-2, 2, 7, 4}, {-3, 2, 7, 4}, {3, 2, 7, 4},
		{-4, 3, 2, 3}, {9, 1, 4, 6}, {-9, 1, 4, 6}, {-4, math.MaxInt, 1, 1}, {0, math.MaxInt, 1, 1},
	}
	for _, tc := range cases {
		lo, hi := tapRange(tc.off, tc.stride, tc.size, tc.outSize)
		for o := range tc.outSize {
			// o*stride overflows for the huge stride unless o is 0, which is
			// the only position those cases have.
			in := o*tc.stride + tc.off
			inside := in >= 0 && in < tc.size
			if got := o >= lo && o < hi; got != inside {
				t.Errorf("tapRange(%d, %d, %d, %d) = [%d, %d): position %d included = %v, want %v",
					tc.off, tc.stride, tc.size, tc.outSize, lo, hi, o, got, inside)
			}
		}
	}
}
