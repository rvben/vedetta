//go:build !darwin || !cgo

package detect

import "errors"

// coreMLBackendCompiledIn reports that this build has no Core ML backend.
const coreMLBackendCompiledIn = false

// CoreMLBackend is a stub on builds without Core ML: every platform but macOS,
// and macOS built without cgo.
type CoreMLBackend struct{}

// NewCoreMLBackend returns an error on builds without Core ML.
func NewCoreMLBackend(_ string) (*CoreMLBackend, error) {
	return nil, errCoreMLNotCompiledIn
}

func (b *CoreMLBackend) Run(_ []float32) ([]float32, error) {
	return nil, errors.New("core ML not available")
}

func (b *CoreMLBackend) Close() {}

func (b *CoreMLBackend) Name() string { return "Core ML [not compiled]" }

func compileCoreMLModel(_, _ string) error { return errCoreMLNotCompiledIn }

func coreMLDefaultModelSupported() bool { return false }
