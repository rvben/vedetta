//go:build darwin && cgo

package detect

/*
#cgo CFLAGS: -fobjc-arc
#cgo LDFLAGS: -framework CoreML -framework Foundation
#include <stdlib.h>
#include "coreml_darwin.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

// coreMLBackendCompiledIn reports that this build carries the Core ML backend.
const coreMLBackendCompiledIn = true

// CoreMLBackend runs the model through Apple's Core ML framework, which
// executes it on the GPU.
//
// Not safe for concurrent use. Each goroutine needs its own instance.
type CoreMLBackend struct {
	handle unsafe.Pointer
	output []float32
}

// NewCoreMLBackend loads a compiled Core ML model (a .mlmodelc directory).
func NewCoreMLBackend(compiledModelPath string) (*CoreMLBackend, error) {
	// The layouts prepareInputInto writes and processOutput reads.
	handle, err := openCoreMLModel(compiledModelPath,
		[]int{1, 3, modelInputSize, modelInputSize},
		[]int{1, 4 + numClasses, numDetections})
	if err != nil {
		return nil, err
	}
	return &CoreMLBackend{handle: handle, output: make([]float32, yoloOutputSize)}, nil
}

// openCoreMLModel loads a compiled model that maps one float32 array of
// inputShape to one of outputShape, and refuses any other.
func openCoreMLModel(compiledModelPath string, inputShape, outputShape []int) (unsafe.Pointer, error) {
	path := C.CString(compiledModelPath)
	defer C.free(unsafe.Pointer(path))

	in := make([]C.size_t, len(inputShape))
	for i, dim := range inputShape {
		in[i] = C.size_t(dim)
	}
	out := make([]C.size_t, len(outputShape))
	for i, dim := range outputShape {
		out[i] = C.size_t(dim)
	}

	var cerr *C.char
	handle := C.vd_coreml_open(path, &in[0], C.size_t(len(in)), &out[0], C.size_t(len(out)), &cerr)
	if handle == nil {
		return nil, coreMLError(cerr)
	}
	return handle, nil
}

// coreMLDefaultModelSupported reports whether this system can load the default
// model.
func coreMLDefaultModelSupported() bool {
	return C.vd_coreml_default_model_supported() != 0
}

// Run executes inference through Core ML.
func (b *CoreMLBackend) Run(input []float32) ([]float32, error) {
	if b.handle == nil {
		return nil, errors.New("core ML backend is closed")
	}
	if len(input) != inputTensorSize {
		return nil, fmt.Errorf("input size %d, want %d", len(input), inputTensorSize)
	}

	var cerr *C.char
	if C.vd_coreml_run(b.handle, (*C.float)(unsafe.Pointer(&input[0])), (*C.float)(unsafe.Pointer(&b.output[0])), &cerr) != 0 {
		return nil, coreMLError(cerr)
	}
	return b.output, nil
}

// Close releases the loaded model. Safe to call multiple times.
func (b *CoreMLBackend) Close() {
	if b.handle != nil {
		C.vd_coreml_close(b.handle)
		b.handle = nil
	}
}

func (b *CoreMLBackend) Name() string { return "Core ML" }

// compileCoreMLModel compiles the model package at src for this machine and
// stores the result at dst, which must not exist.
func compileCoreMLModel(src, dst string) error {
	csrc := C.CString(src)
	defer C.free(unsafe.Pointer(csrc))
	cdst := C.CString(dst)
	defer C.free(unsafe.Pointer(cdst))

	var cerr *C.char
	if C.vd_coreml_compile(csrc, cdst, &cerr) != 0 {
		return coreMLError(cerr)
	}
	return nil
}

// coreMLError takes ownership of a message returned by the Objective-C side.
func coreMLError(msg *C.char) error {
	if msg == nil {
		return errors.New("core ML failed without a message")
	}
	defer C.free(unsafe.Pointer(msg))
	return errors.New(C.GoString(msg))
}
