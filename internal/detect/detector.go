package detect

import (
	"errors"
	"fmt"
	"image"
	"log/slog"
	"os"
	"sync"

	"github.com/rvben/vedetta/internal/config"
)

// Detection represents a single detected object.
type Detection struct {
	Label string
	Score float32
	Box   [4]int // x1, y1, x2, y2
}

// Detector runs object detection on image frames.
// It selects the best available backend automatically.
// Safe for concurrent use by multiple camera goroutines.
type Detector struct {
	mu           sync.Mutex
	config       config.DetectConfig
	backend      Backend
	enabled      bool
	inputBuf     []float32       // reusable preprocessing buffer [3*640*640], guarded by mu
	labelAllowed map[string]bool // nil = allow all labels
}

func New(cfg config.DetectConfig) *Detector {
	d := &Detector{
		config: cfg,
	}

	if len(cfg.Labels) > 0 {
		d.labelAllowed = make(map[string]bool, len(cfg.Labels))
		for _, l := range cfg.Labels {
			d.labelAllowed[l] = true
		}
		slog.Info("label filter active", "labels", cfg.Labels)
	}

	if err := d.init(cfg); err != nil {
		slog.Warn("object detection unavailable, using motion-only mode",
			"reason", err.Error(),
		)
		return d
	}

	d.enabled = true
	slog.Info("object detection initialized", "backend", d.backend.Name())

	return d
}

func (d *Detector) filterLabels(dets []Detection) []Detection {
	if d.labelAllowed == nil {
		return dets
	}
	filtered := dets[:0]
	for _, det := range dets {
		if d.labelAllowed[det.Label] {
			filtered = append(filtered, det)
		}
	}
	return filtered
}

func (d *Detector) MotionThreshold() float64 {
	return d.config.Motion.MinRegionScore
}

func (d *Detector) Available() bool {
	return d != nil && d.enabled
}

// Detect runs object detection on a frame and returns detections above threshold.
// Safe for concurrent use — serializes access to the backend.
// Recovers from panics in the inference backend to prevent server crashes.
func (d *Detector) Detect(img *image.RGBA) (result []Detection) {
	if !d.enabled {
		return nil
	}

	defer func() {
		if r := recover(); r != nil {
			slog.Error("inference panic recovered", "error", r)
			result = nil
		}
	}()

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.inputBuf == nil {
		d.inputBuf = make([]float32, 3*modelInputSize*modelInputSize)
	}
	inputData, scale, padX, padY := prepareInputInto(d.inputBuf, img)

	output, err := d.backend.Run(inputData)
	if err != nil {
		slog.Error("inference failed", "error", err)
		return nil
	}

	return d.filterLabels(processOutput(output, d.config.ScoreThreshold, scale, padX, padY))
}

// DetectRGB24 runs object detection directly on RGB24 frame data,
// avoiding the intermediate RGBA conversion.
// Safe for concurrent use — serializes access to the backend.
// Recovers from panics in the inference backend to prevent server crashes.
func (d *Detector) DetectRGB24(data []byte, w, h int) (result []Detection) {
	if !d.enabled {
		return nil
	}

	defer func() {
		if r := recover(); r != nil {
			slog.Error("inference panic recovered", "error", r)
			result = nil
		}
	}()

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.inputBuf == nil {
		d.inputBuf = make([]float32, 3*modelInputSize*modelInputSize)
	}
	inputData, scale, padX, padY := prepareInputFromRGB24Into(d.inputBuf, data, w, h)

	output, err := d.backend.Run(inputData)
	if err != nil {
		slog.Error("inference failed", "error", err)
		return nil
	}

	return d.filterLabels(processOutput(output, d.config.ScoreThreshold, scale, padX, padY))
}

// SetScoreThreshold updates the score threshold for detection. Thread-safe.
func (d *Detector) SetScoreThreshold(t float32) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.config.ScoreThreshold = t
}

// ScoreThreshold returns the current score threshold. Thread-safe.
func (d *Detector) ScoreThreshold() float32 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.config.ScoreThreshold
}

// SetLabels updates the label filter. Pass nil or empty to allow all labels. Thread-safe.
func (d *Detector) SetLabels(labels []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(labels) == 0 {
		d.labelAllowed = nil
		return
	}
	allowed := make(map[string]bool, len(labels))
	for _, l := range labels {
		allowed[l] = true
	}
	d.labelAllowed = allowed
}

// Labels returns the current label filter. Returns nil if all labels are allowed. Thread-safe.
func (d *Detector) Labels() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.labelAllowed == nil {
		return nil
	}
	labels := make([]string, 0, len(d.labelAllowed))
	for l := range d.labelAllowed {
		labels = append(labels, l)
	}
	return labels
}

func (d *Detector) Close() {
	if d.backend != nil {
		d.backend.Close()
	}
}

func (d *Detector) init(cfg config.DetectConfig) error {
	modelData, err := d.loadModelData(cfg.ModelPath)
	if err != nil {
		return fmt.Errorf("load model: %w", err)
	}

	backend, err := selectBackend(cfg.Backend, modelData)
	if err != nil {
		return err
	}

	d.backend = backend
	return nil
}

// errCAPINotCompiledIn reports a C ONNX Runtime backend that this binary cannot
// provide at all, as opposed to one that is present but failed to start.
var errCAPINotCompiledIn = errors.New("c ONNX Runtime not available: build with -tags cgo_onnxruntime")

// selectBackend picks the best available backend based on config and build tags.
//
// Whether the C backend exists is settled at compile time by
// capiBackendCompiledIn, so a build without it never calls a constructor that
// cannot succeed: the explicit request fails with the reason, and auto-selection
// goes straight to the pure Go backend instead of probing and logging every
// start.
func selectBackend(preference string, modelData []byte) (Backend, error) {
	switch preference {
	case "go":
		return NewGoBackend(modelData)

	case "onnxruntime_c":
		if !capiBackendCompiledIn {
			return nil, fmt.Errorf("c ONNX Runtime backend: %w", errCAPINotCompiledIn)
		}
		b, err := NewCAPIBackend(modelData)
		if err != nil {
			return nil, fmt.Errorf("c ONNX Runtime backend: %w", err)
		}
		return b, nil

	case "", "auto":
		// Prefer C ONNX Runtime when it is compiled in (faster), fall back to
		// pure Go when it is absent or fails to initialize.
		if capiBackendCompiledIn {
			b, err := NewCAPIBackend(modelData)
			if err == nil {
				slog.Info("auto-selected C ONNX Runtime backend")
				return b, nil
			}
			slog.Info("C ONNX Runtime not available, using pure Go backend", "reason", err.Error())
		}
		return NewGoBackend(modelData)

	default:
		return nil, fmt.Errorf("unknown backend %q: use \"auto\", \"go\", or \"onnxruntime_c\"", preference)
	}
}

// loadModelData resolves model bytes from config path, embedded data, common locations, or auto-download.
func (d *Detector) loadModelData(modelPath string) ([]byte, error) {
	if modelPath != "" {
		slog.Info("loading model from path", "path", modelPath)
		data, err := os.ReadFile(modelPath)
		if err == nil {
			return data, nil
		}
		slog.Warn("configured model not found, will try cache/download", "path", modelPath, "error", err)
	}

	if len(embeddedModel) > 0 {
		slog.Info("using embedded model")
		return embeddedModel, nil
	}

	// Check common local paths
	candidates := []string{
		"yolov8n.onnx",
	}
	for _, path := range candidates {
		if data, err := os.ReadFile(path); err == nil {
			slog.Info("found model at", "path", path)
			return data, nil
		}
	}
	if data, err := readVerifiedCachedModel(cachedModelPath()); err == nil {
		slog.Info("found verified cached model at", "path", cachedModelPath())
		return data, nil
	} else if err != nil && !os.IsNotExist(err) {
		slog.Warn("cached model failed verification, re-downloading", "path", cachedModelPath(), "error", err)
	}

	// Auto-download as last resort
	path, err := downloadModel()
	if err != nil {
		return nil, fmt.Errorf("auto-download model: %w", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read downloaded model: %w", err)
	}
	return data, nil
}
