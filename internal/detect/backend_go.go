package detect

import (
	"fmt"
	"runtime"

	"github.com/rvben/vedetta/internal/detect/onnxruntime"
)

// GoBackend is the pure Go ONNX inference engine. It requires no external
// dependencies and works on every platform Go supports.
//
// Not safe for concurrent use. Each goroutine needs its own instance.
type GoBackend struct {
	session   *onnxruntime.Session
	inputMap  map[string]*onnxruntime.Tensor
	inputKey  string
	outputKey string
}

// NewGoBackend loads an ONNX model and returns a pure Go inference backend.
func NewGoBackend(modelData []byte) (*GoBackend, error) {
	session, err := onnxruntime.NewSession(modelData)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	inputNames := session.InputNames()
	outputNames := session.OutputNames()
	if len(inputNames) == 0 || len(outputNames) == 0 {
		return nil, fmt.Errorf("model has no inputs or outputs")
	}

	inputKey := inputNames[0]
	b := &GoBackend{
		session:   session,
		inputKey:  inputKey,
		outputKey: outputNames[0],
		inputMap:  map[string]*onnxruntime.Tensor{inputKey: nil},
	}

	if err := b.warmUp(); err != nil {
		return nil, err
	}
	return b, nil
}

// warmUp runs one inference on a blank frame so that a model this detector
// cannot use is rejected while the backend is being built. A model that loads
// but cannot run otherwise fails once per frame inside the panic recovery in
// Detect, where it reads as a camera that simply sees nothing.
func (b *GoBackend) warmUp() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("model warm-up panicked: %v", r)
		}
	}()

	output, err := b.Run(make([]float32, inputTensorSize))
	if err != nil {
		return fmt.Errorf("model warm-up: %w", err)
	}
	if len(output) != yoloOutputSize {
		return fmt.Errorf("model warm-up: output has %d elements, want %d for the YOLOv8 (1,84,8400) layout",
			len(output), yoloOutputSize)
	}
	return nil
}

// Run executes inference using the pure Go ONNX runtime.
func (b *GoBackend) Run(input []float32) ([]float32, error) {
	if len(input) != inputTensorSize {
		return nil, fmt.Errorf("input size %d, want %d", len(input), inputTensorSize)
	}

	inputTensor := onnxruntime.NewTensor(
		[]int64{1, 3, modelInputSize, modelInputSize}, input,
	)

	// Reuse the map — only update the value pointer.
	b.inputMap[b.inputKey] = inputTensor

	outputs, err := b.session.Run(b.inputMap)
	if err != nil {
		return nil, err
	}

	output, ok := outputs[b.outputKey]
	if !ok {
		return nil, fmt.Errorf("model produced no %q tensor", b.outputKey)
	}

	return output.Data, nil
}

// Close is a no-op for the pure Go backend (no external resources).
func (b *GoBackend) Close() {}

// Name returns the backend identifier including BLAS info.
func (b *GoBackend) Name() string {
	if runtime.GOOS == "darwin" {
		return "pure Go + Apple Accelerate BLAS"
	}
	return "pure Go"
}
