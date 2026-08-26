//go:build !cgo_onnxruntime

package detect

import "fmt"

// capiBackendCompiledIn reports that this build has no C ONNX Runtime backend.
const capiBackendCompiledIn = false

// CAPIBackend is a stub when built without the cgo_onnxruntime tag.
// All methods return errors indicating the backend is unavailable.
type CAPIBackend struct{}

// NewCAPIBackend returns an error when built without the cgo_onnxruntime tag.
func NewCAPIBackend(_ []byte) (*CAPIBackend, error) {
	return nil, errCAPINotCompiledIn
}

func (b *CAPIBackend) Run(_ []float32) ([]float32, error) {
	return nil, fmt.Errorf("c ONNX Runtime not available")
}

func (b *CAPIBackend) Close() {}

func (b *CAPIBackend) Name() string { return "ONNX Runtime (C API) [not compiled]" }
