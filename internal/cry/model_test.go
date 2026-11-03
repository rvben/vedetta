package cry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/rvben/vedetta/internal/detect/onnxruntime"
)

func TestReferenceParity(t *testing.T) {
	dir := os.Getenv("VEDETTA_CRY_MODEL")
	fixtures := os.Getenv("VEDETTA_CRY_FIXTURES")
	if dir == "" || fixtures == "" {
		t.Skip("set VEDETTA_CRY_MODEL and VEDETTA_CRY_FIXTURES for official-model parity")
	}
	path := filepath.Join(dir, "manifest.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(b)
	m, err := Load(path, hex.EncodeToString(hash[:]))
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(fixtures, "*.json"))
	if err != nil || len(files) < 50 {
		t.Fatalf("need 50 reference fixtures: %v", err)
	}
	var maxMel, maxScore float64
	for _, file := range files {
		var f struct {
			Name   string
			PCM    []int16
			Scores [][]float32
			Mel    [][]float32
		}
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatal(err)
		}
		patches := m.features(f.PCM)
		if len(patches) != len(f.Scores) {
			t.Fatalf("%s: patch count mismatch", f.Name)
		}
		for p, patch := range patches {
			for i, v := range patch {
				delta := math.Abs(float64(v - f.Mel[p*48+i/64][i%64]))
				maxMel = math.Max(maxMel, delta)
				// Float32 reference FFTs differ most near the mel noise floor;
				// retain a tight end-to-end score tolerance on every class below.
				if delta > 3e-3 {
					t.Fatalf("%s mel error %g", f.Name, delta)
				}
			}
			out, err := m.session.Run(map[string]*onnxruntime.Tensor{"mel": onnxruntime.NewTensor([]int64{1, 1, 96, 64}, patch)})
			if err != nil {
				t.Fatal(err)
			}
			for i, v := range out["scores"].Data {
				delta := math.Abs(float64(v - f.Scores[p][i]))
				maxScore = math.Max(maxScore, delta)
				if delta > 1e-4 {
					t.Fatalf("%s class %d parity error %g", f.Name, i, delta)
				}
			}
		}
	}
	t.Logf("50 fixtures: max mel error=%g max score error=%g", maxMel, maxScore)
}

func TestFFT(t *testing.T) {
	a := make([]complex128, 512)
	a[0] = 1
	fft(a)
	for _, v := range a {
		if v != 1 {
			t.Fatal("impulse spectrum mismatch")
		}
	}
}

func TestModelRejectsUnpinnedManifest(t *testing.T) {
	p := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(p, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p, ""); err == nil {
		t.Fatal("accepted unpinned model")
	}
}
