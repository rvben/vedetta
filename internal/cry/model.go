// Package cry implements optional local infant-cry recognition. Scores are
// uncalibrated model outputs, never probabilities of a child's wellbeing.
package cry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// constantLevels bounds how many distinct constant windows a model remembers.
// It holds every level G.711 can decode to, 256 each for A-law and mu-law, so
// camera audio never fills it; the bound is for callers that score other PCM.
const constantLevels = 512

// Model owns one session and serializes its own inference.
type Model struct {
	mu       sync.Mutex
	session  *onnxruntime.Session
	manifest manifest
	frontend frontend

	// constant holds the model's own score for windows in which every sample
	// has the same value, keyed by that value. Inference is deterministic, so
	// such a window is answered from here without running the model again.
	constantMu sync.Mutex
	constant   map[int16]float32
}

// constantLevel reports the value of a window whose samples are all equal.
func constantLevel(pcm []int16) (int16, bool) {
	level := pcm[0]
	for _, s := range pcm[1:] {
		if s != level {
			return 0, false
		}
	}
	return level, true
}

// remembered returns the stored score of a constant window: constant says
// whether every sample equals level, ok whether that level's score is stored.
func (m *Model) remembered(pcm []int16) (score float32, level int16, constant, ok bool) {
	level, constant = constantLevel(pcm)
	if !constant {
		return 0, 0, false, false
	}
	m.constantMu.Lock()
	defer m.constantMu.Unlock()
	score, ok = m.constant[level]
	return score, level, true, ok
}

func (m *Model) remember(level int16, score float32) {
	m.constantMu.Lock()
	defer m.constantMu.Unlock()
	if m.constant == nil {
		m.constant = make(map[int16]float32, constantLevels)
	}
	if len(m.constant) < constantLevels {
		m.constant[level] = score
	}
}

// windowSamples is two seconds of 8 kHz audio, the only length the model takes.
const windowSamples = 16000

var errWindowLength = errors.New("YAMNet requires two seconds of 8 kHz mono PCM")

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
	m := &Model{session: session, manifest: meta, frontend: newFrontend(meta)}
	if _, err := m.Score(make([]int16, windowSamples)); err != nil {
		return nil, fmt.Errorf("cry model warmup: %w", err)
	}
	return m, nil
}

// Score returns the cry score of one two-second window of 8 kHz mono PCM.
func (m *Model) Score(pcm []int16) (float32, error) {
	return m.scoreWith(pcm, func() bool { m.mu.Lock(); return true })
}

// TryScore sheds work while another camera uses the shared model. It never
// builds an inference backlog and keeps only one active session's tensors.
// A remembered constant window needs no session, so it is answered even then.
func (m *Model) TryScore(pcm []int16) (float32, error) {
	return m.scoreWith(pcm, m.mu.TryLock)
}

func (m *Model) scoreWith(pcm []int16, lock func() bool) (float32, error) {
	if len(pcm) != windowSamples {
		return 0, errWindowLength
	}
	score, level, constant, ok := m.remembered(pcm)
	if ok {
		return score, nil
	}
	if !lock() {
		return 0, ErrBusy
	}
	defer m.mu.Unlock()
	score, err := m.infer(pcm)
	if err == nil && constant {
		m.remember(level, score)
	}
	return score, err
}

// infer runs the model on one window. The caller holds mu.
func (m *Model) infer(pcm []int16) (score float32, err error) {
	defer func() {
		if r := recover(); r != nil {
			score = 0
			err = fmt.Errorf("cry inference failed: %v", r)
		}
	}()
	if len(pcm) != windowSamples {
		return 0, errWindowLength
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
