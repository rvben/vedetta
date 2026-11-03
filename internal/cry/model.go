// Package cry implements optional local infant-cry recognition. Scores are
// uncalibrated model outputs, never probabilities of a child's wellbeing.
package cry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"

	"github.com/rvben/vedetta/internal/detect/onnxruntime"
)

type manifest struct {
	Version      int       `json:"version"`
	Model        string    `json:"model"`
	SHA256       string    `json:"sha256"`
	SourceSHA256 string    `json:"source_sha256"`
	CryIndex     int       `json:"cry_index"`
	Mel          []float32 `json:"mel_weights"`
	Resample     []float64 `json:"resample"`
}

// Model owns one session. Callers must serialize Score calls.
type Model struct {
	mu       sync.Mutex
	session  *onnxruntime.Session
	manifest manifest
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("model asset exceeds size limit")
	}
	return b, nil
}

func matches(data []byte, expected string) bool {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) == expected
}

// Load requires the operator-pinned manifest digest. The manifest pins both
// inference weights and exact TensorFlow mel/SciPy resampling coefficients.
func Load(path, digest string) (*Model, error) {
	b, err := readBounded(path, 1<<20)
	if err != nil {
		return nil, err
	}
	if !matches(b, digest) {
		return nil, fmt.Errorf("cry model manifest checksum mismatch")
	}
	var meta manifest
	if err := json.Unmarshal(b, &meta); err != nil {
		return nil, err
	}
	if meta.Version != 1 || meta.Model != "yamnet.onnx" || meta.CryIndex != 20 || len(meta.Mel) != 257*64 || len(meta.Resample) != 41 {
		return nil, fmt.Errorf("unsupported YAMNet bundle")
	}
	for _, v := range meta.Mel {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) || v < 0 {
			return nil, fmt.Errorf("invalid mel weights")
		}
	}
	for _, v := range meta.Resample {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("invalid resampling weights")
		}
	}
	data, err := readBounded(filepath.Join(filepath.Dir(path), meta.Model), 32<<20)
	if err != nil {
		return nil, err
	}
	if !matches(data, meta.SHA256) {
		return nil, fmt.Errorf("cry model checksum mismatch")
	}
	session, err := onnxruntime.NewSession(data)
	if err != nil {
		return nil, err
	}
	if names := session.InputNames(); len(names) != 1 || names[0] != "mel" {
		return nil, fmt.Errorf("unexpected cry model input")
	}
	if names := session.OutputNames(); len(names) != 1 || names[0] != "scores" {
		return nil, fmt.Errorf("unexpected cry model output")
	}
	m := &Model{session: session, manifest: meta}
	if _, err := m.Score(make([]int16, 16000)); err != nil {
		return nil, fmt.Errorf("cry model warmup: %w", err)
	}
	return m, nil
}

func (m *Model) Score(pcm []int16) (score float32, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.score(pcm)
}

// TryScore sheds work while another camera uses the shared model. It never
// builds an inference backlog and keeps only one active session's tensors.
func (m *Model) TryScore(pcm []int16) (float32, error) {
	if !m.mu.TryLock() {
		return 0, ErrBusy
	}
	defer m.mu.Unlock()
	return m.score(pcm)
}

func (m *Model) score(pcm []int16) (score float32, err error) {
	defer func() {
		if r := recover(); r != nil {
			score = 0
			err = fmt.Errorf("cry inference failed: %v", r)
		}
	}()
	if len(pcm) != 16000 {
		return 0, fmt.Errorf("YAMNet requires two seconds of 8 kHz mono PCM")
	}
	patches := m.features(pcm)
	for _, patch := range patches {
		out, err := m.session.Run(map[string]*onnxruntime.Tensor{"mel": onnxruntime.NewTensor([]int64{1, 1, 96, 64}, patch)})
		if err != nil {
			return 0, err
		}
		t := out["scores"]
		if t == nil || len(t.Data) != 521 {
			return 0, fmt.Errorf("unexpected YAMNet output")
		}
		for _, v := range t.Data {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) || v < 0 || v > 1 {
				return 0, fmt.Errorf("invalid YAMNet score")
			}
		}
		score += t.Data[m.manifest.CryIndex]
	}
	return score / float32(len(patches)), nil
}
