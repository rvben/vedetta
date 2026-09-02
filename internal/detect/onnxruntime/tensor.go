// Package onnxruntime provides a pure Go ONNX model inference engine.
// It supports the subset of ONNX operators needed by the models vedetta runs:
// YOLOv8 object detection, SCRFD face detection, FaceNet face embedding and
// OSNet object re-identification. On macOS it uses Apple Accelerate for
// hardware-optimized matrix multiplication.
//
// The engine is generic over the operators it implements, but the callers in
// package detect decode model output for those specific architectures and
// reject any other output shape.
package onnxruntime

import (
	"fmt"
	"math"
	"math/bits"
	"sync"
)

// Tensor is an N-dimensional array of float32 values in row-major order.
type Tensor struct {
	Data  []float32
	Shape []int64
	// pooled indicates the Data was allocated via getTensorData and can be returned.
	pooled bool
}

// tensorFreeLists provides persistent float32 buffer pools bucketed by power-of-2 sizes.
// Unlike sync.Pool, these survive GC cycles, so repeated inference runs reuse buffers.
var tensorFreeLists [32]struct {
	mu   sync.Mutex
	bufs [][]float32
}

func getTensorData(size int) []float32 {
	if size <= 0 {
		return nil
	}
	bucket := bucketFor(size)
	fl := &tensorFreeLists[bucket]
	fl.mu.Lock()
	if n := len(fl.bufs); n > 0 {
		buf := fl.bufs[n-1]
		fl.bufs = fl.bufs[:n-1]
		fl.mu.Unlock()
		buf = buf[:size]
		for i := range buf {
			buf[i] = 0
		}
		return buf
	}
	fl.mu.Unlock()
	return make([]float32, size, 1<<bucket)
}

func putTensorData(buf []float32) {
	if cap(buf) == 0 {
		return
	}
	bucket := bucketFor(cap(buf))
	fl := &tensorFreeLists[bucket]
	fl.mu.Lock()
	fl.bufs = append(fl.bufs, buf[:cap(buf)])
	fl.mu.Unlock()
}

func bucketFor(size int) int {
	if size <= 1 {
		return 0
	}
	return bits.Len(uint(size - 1))
}

// NewTensor creates a tensor with the given shape and data.
// If data is nil, a zero-filled tensor is allocated from the pool.
func NewTensor(shape []int64, data []float32) *Tensor {
	size := tensorSize(shape)
	if data == nil {
		data = getTensorData(int(size))
		return &Tensor{Data: data, Shape: shape, pooled: true}
	}
	return &Tensor{Data: data, Shape: shape}
}

// newTensorUninit allocates a tensor from the pool without zeroing.
// The caller MUST write every element before reading.
func newTensorUninit(shape []int64) *Tensor {
	size := int(tensorSize(shape))
	if size <= 0 {
		return &Tensor{Shape: shape}
	}
	bucket := bucketFor(size)
	fl := &tensorFreeLists[bucket]
	fl.mu.Lock()
	var data []float32
	if n := len(fl.bufs); n > 0 {
		data = fl.bufs[n-1][:size]
		fl.bufs = fl.bufs[:n-1]
		fl.mu.Unlock()
	} else {
		fl.mu.Unlock()
		data = make([]float32, size, 1<<bucket)
	}
	return &Tensor{Data: data, Shape: shape, pooled: true}
}

// ScalarTensor creates a 0-dimensional tensor with a single value.
func ScalarTensor(v float32) *Tensor {
	return &Tensor{Data: []float32{v}, Shape: []int64{}}
}

// tensorSize returns the total number of elements for a given shape.
func tensorSize(shape []int64) int64 {
	if len(shape) == 0 {
		return 1
	}
	n := int64(1)
	for _, d := range shape {
		n *= d
	}
	return n
}

// Size returns the total number of elements.
func (t *Tensor) Size() int {
	return int(tensorSize(t.Shape))
}

// Dims returns the number of dimensions.
func (t *Tensor) Dims() int {
	return len(t.Shape)
}

// Clone returns a deep copy.
func (t *Tensor) Clone() *Tensor {
	data := make([]float32, len(t.Data))
	copy(data, t.Data)
	shape := make([]int64, len(t.Shape))
	copy(shape, t.Shape)
	return &Tensor{Data: data, Shape: shape}
}

// Strides returns the row-major strides for each dimension.
func (t *Tensor) Strides() []int64 {
	n := len(t.Shape)
	strides := make([]int64, n)
	if n == 0 {
		return strides
	}
	strides[n-1] = 1
	for i := n - 2; i >= 0; i-- {
		strides[i] = strides[i+1] * t.Shape[i+1]
	}
	return strides
}

// Reshape returns a view with a new shape (same underlying data).
// One dimension may be -1 to infer from the total size.
func (t *Tensor) Reshape(shape []int64) (*Tensor, error) {
	resolved, err := resolveShape(shape, int64(len(t.Data)))
	if err != nil {
		return nil, err
	}
	return &Tensor{Data: t.Data, Shape: resolved}, nil
}

// resolveShape replaces a -1 dimension with the inferred value.
func resolveShape(shape []int64, totalSize int64) ([]int64, error) {
	out := make([]int64, len(shape))
	copy(out, shape)

	inferIdx := -1
	product := int64(1)
	for i, d := range out {
		switch d {
		case -1:
			if inferIdx != -1 {
				return nil, fmt.Errorf("reshape: multiple -1 dimensions")
			}
			inferIdx = i
		case 0:
			return nil, fmt.Errorf("reshape: zero dimension at index %d", i)
		default:
			product *= d
		}
	}

	if inferIdx != -1 {
		if product == 0 {
			return nil, fmt.Errorf("reshape: cannot infer dimension with zero product")
		}
		out[inferIdx] = totalSize / product
		product *= out[inferIdx]
	}

	if product != totalSize {
		return nil, fmt.Errorf("reshape: shape %v incompatible with size %d", out, totalSize)
	}
	return out, nil
}

// broadcastShapes computes the broadcast-compatible output shape for two input shapes.
func broadcastShapes(a, b []int64) ([]int64, error) {
	maxDims := len(a)
	if len(b) > maxDims {
		maxDims = len(b)
	}

	result := make([]int64, maxDims)
	for i := range result {
		da := int64(1)
		if idx := len(a) - maxDims + i; idx >= 0 {
			da = a[idx]
		}
		db := int64(1)
		if idx := len(b) - maxDims + i; idx >= 0 {
			db = b[idx]
		}

		if da == db {
			result[i] = da
		} else if da == 1 {
			result[i] = db
		} else if db == 1 {
			result[i] = da
		} else {
			return nil, fmt.Errorf("broadcast: incompatible shapes %v and %v", a, b)
		}
	}
	return result, nil
}

// broadcastIndex maps a flat index in the output tensor to the corresponding
// flat index in a (possibly broadcast) input tensor.
func broadcastIndex(flatIdx int64, outShape, inShape []int64) int64 {
	// Decompose flatIdx into multi-dimensional indices for outShape,
	// clamp each to inShape, then re-compose.
	n := len(outShape)
	offset := len(outShape) - len(inShape)
	idx := int64(0)
	stride := int64(1)

	for i := n - 1; i >= 0; i-- {
		dimSize := outShape[i]
		coord := (flatIdx % dimSize)
		flatIdx /= dimSize

		inIdx := i - offset
		if inIdx >= 0 {
			inDim := inShape[inIdx]
			if inDim == 1 {
				coord = 0
			}
			idx += coord * stride
			stride *= inDim
		}
	}
	return idx
}

// Transpose returns a new tensor with permuted dimensions.
func (t *Tensor) Transpose(perm []int64) *Tensor {
	ndim := len(t.Shape)
	if len(perm) != ndim {
		return t.Clone()
	}

	newShape := make([]int64, ndim)
	for i, p := range perm {
		newShape[i] = t.Shape[p]
	}

	oldStrides := t.Strides()
	result := newTensorUninit(newShape)
	newSize := int(tensorSize(newShape))

	newStrides := make([]int64, ndim)
	if ndim > 0 {
		newStrides[ndim-1] = 1
		for i := ndim - 2; i >= 0; i-- {
			newStrides[i] = newStrides[i+1] * newShape[i+1]
		}
	}

	for i := range newSize {
		remaining := int64(i)
		oldIdx := int64(0)
		for d := range ndim {
			coord := remaining / newStrides[d]
			remaining %= newStrides[d]
			oldIdx += coord * oldStrides[perm[d]]
		}
		result.Data[i] = t.Data[oldIdx]
	}

	return result
}

// Fill sets all elements to the given value.
func (t *Tensor) Fill(v float32) {
	for i := range t.Data {
		t.Data[i] = v
	}
}

// Max returns the maximum value.
func (t *Tensor) Max() float32 {
	if len(t.Data) == 0 {
		return float32(math.Inf(-1))
	}
	m := t.Data[0]
	for _, v := range t.Data[1:] {
		if v > m {
			m = v
		}
	}
	return m
}
