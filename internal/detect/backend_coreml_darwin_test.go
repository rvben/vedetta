//go:build darwin && cgo

package detect

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// coreMLTestArchive is where a developer keeps the default model archive for
// these tests (make coreml-model writes it there). It is not in the repository,
// so the tests that need it skip without it.
const coreMLTestArchive = "models/" + coreMLArchiveName

// compileTestCoreMLModel unpacks the model archive and compiles it through the
// same steps the backend takes, returning the compiled model's path.
func compileTestCoreMLModel(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat(coreMLTestArchive); err != nil {
		t.Skipf("%s not found, skipping", coreMLTestArchive)
	}
	if !coreMLDefaultModelSupported() {
		t.Skip("the default model needs macOS 15 or later")
	}

	unpacked := filepath.Join(t.TempDir(), "package")
	if err := unzipInto(coreMLTestArchive, unpacked, maxArchiveBytes); err != nil {
		t.Fatalf("unpack model: %v", err)
	}
	packages, err := filepath.Glob(filepath.Join(unpacked, "*"+coreMLPackageExt))
	if err != nil || len(packages) != 1 {
		t.Fatalf("archive holds %d model packages (%v), want 1", len(packages), err)
	}
	compiled, err := prepareCoreMLModel(packages[0], t.TempDir(), compileCoreMLModel, nil)
	if err != nil {
		t.Fatalf("compile model: %v", err)
	}
	return compiled
}

// testInput fills a model input with a fixed pattern that varies across the
// image and between channels.
func testInput() []float32 {
	input := make([]float32, inputTensorSize)
	state := uint32(1)
	for i := range input {
		state = state*1664525 + 1013904223
		input[i] = float32(state>>8) / float32(1<<24)
	}
	return input
}

// The archive the backend downloads is pinned by checksum, and the parity test
// below runs whatever archive is on disk. This ties the two together: parity
// proven on a different export says nothing about the pinned one.
func TestCoreMLTestArchiveIsThePinnedOne(t *testing.T) {
	data, err := os.ReadFile(coreMLTestArchive)
	if err != nil {
		t.Skipf("%s not found, skipping", coreMLTestArchive)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != coreMLArchiveSHA256 {
		t.Fatalf("%s has sha256 %s, the backend pins %s", coreMLTestArchive, got, coreMLArchiveSHA256)
	}
}

func TestCoreMLBackend_MatchesGoBackend(t *testing.T) {
	modelData := loadTestModel(t)
	compiled := compileTestCoreMLModel(t)

	reference, err := NewGoBackend(modelData)
	if err != nil {
		t.Fatalf("NewGoBackend: %v", err)
	}
	defer reference.Close()
	backend, err := NewCoreMLBackend(compiled)
	if err != nil {
		t.Fatalf("NewCoreMLBackend: %v", err)
	}
	defer backend.Close()

	input := testInput()
	want, err := reference.Run(input)
	if err != nil {
		t.Fatalf("go backend: %v", err)
	}
	got, err := backend.Run(input)
	if err != nil {
		t.Fatalf("core ML backend: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("output has %d values, want %d", len(got), len(want))
	}

	// Box coordinates run to 640 and class scores to 1, and the two runtimes
	// order their float32 arithmetic differently, so the outputs agree to a
	// rounding margin rather than exactly.
	const tolerance = 0.01
	var worst float64
	var energy float64
	for i := range want {
		// A NaN compares false against any bound, so it has to be refused by
		// name or it would pass as agreement.
		if v := float64(got[i]); math.IsNaN(v) || math.IsInf(v, 0) {
			t.Fatalf("output value %d is %v", i, got[i])
		}
		worst = math.Max(worst, math.Abs(float64(got[i])-float64(want[i])))
		energy += math.Abs(float64(want[i]))
	}
	if energy == 0 {
		t.Fatal("reference output is all zeros, so agreeing with it proves nothing")
	}
	if worst > tolerance {
		t.Fatalf("outputs differ by up to %g, want at most %g", worst, tolerance)
	}
	t.Logf("largest difference from the Go backend: %g", worst)
}

// The returned slice is reused, so a second run must overwrite it with the new
// result rather than leave the first one in place.
func TestCoreMLBackend_SecondRunReflectsItsOwnInput(t *testing.T) {
	backend, err := NewCoreMLBackend(compileTestCoreMLModel(t))
	if err != nil {
		t.Fatalf("NewCoreMLBackend: %v", err)
	}
	defer backend.Close()

	first, err := backend.Run(testInput())
	if err != nil {
		t.Fatal(err)
	}
	firstCopy := append([]float32(nil), first...)

	second, err := backend.Run(make([]float32, inputTensorSize))
	if err != nil {
		t.Fatal(err)
	}
	for i := range second {
		if second[i] != firstCopy[i] {
			return
		}
	}
	t.Fatal("a different input produced an identical output")
}

func TestCoreMLBackend_RejectsBadUse(t *testing.T) {
	backend, err := NewCoreMLBackend(compileTestCoreMLModel(t))
	if err != nil {
		t.Fatalf("NewCoreMLBackend: %v", err)
	}

	if _, err := backend.Run(make([]float32, inputTensorSize-1)); err == nil {
		t.Fatal("a short input was accepted")
	}

	backend.Close()
	backend.Close()
	if _, err := backend.Run(make([]float32, inputTensorSize)); err == nil {
		t.Fatal("a closed backend ran")
	}
}

func TestNewCoreMLBackend_MissingModel(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent"+coreMLCompiledExt)
	if _, err := NewCoreMLBackend(missing); err == nil {
		t.Fatal("a missing model loaded")
	}
}

func TestCompileCoreMLModel_RejectsWhatIsNotAModel(t *testing.T) {
	pkg := writeTestPackage(t, "not a model")
	dst := filepath.Join(t.TempDir(), "out"+coreMLCompiledExt)
	if err := compileCoreMLModel(pkg, dst); err == nil {
		t.Fatal("a directory that is not a model compiled")
	}
	if _, err := os.Stat(dst); err == nil {
		t.Fatal("a failed compile left a compiled model behind")
	}
}

// A model with the right number of values in another arrangement would be read
// in the wrong order, so the layout is checked and not only the size. Asking the
// real model for the transposed head stands in for loading a transposed model.
func TestOpenCoreMLModel_RefusesAnotherLayout(t *testing.T) {
	compiled := compileTestCoreMLModel(t)
	input := []int{1, 3, modelInputSize, modelInputSize}

	handle, err := openCoreMLModel(compiled, input, []int{1, numDetections, 4 + numClasses})
	if err == nil {
		(&CoreMLBackend{handle: handle}).Close()
		t.Fatal("a model with a transposed output was accepted")
	}
	if !strings.Contains(err.Error(), "1,84,8400") {
		t.Fatalf("error %q does not name the model's own layout", err)
	}

	handle, err = openCoreMLModel(compiled, []int{1, 3, modelInputSize, modelInputSize / 2}, []int{1, 4 + numClasses, numDetections})
	if err == nil {
		(&CoreMLBackend{handle: handle}).Close()
		t.Fatal("a model with a different input size was accepted")
	}
}

// Each camera owns a backend and they run at once, so separate instances of
// one compiled model must not hand each other's results back. Every instance
// gets its own input, so a swapped result shows up as a wrong one.
func TestCoreMLBackend_InstancesRunConcurrently(t *testing.T) {
	compiled := compileTestCoreMLModel(t)

	const instances = 4
	const runs = 5
	inputs := make([][]float32, instances)
	want := make([][]float32, instances)
	single, err := NewCoreMLBackend(compiled)
	if err != nil {
		t.Fatalf("NewCoreMLBackend: %v", err)
	}
	for n := range instances {
		inputs[n] = testInput()
		for i := range inputs[n] {
			inputs[n][i] *= float32(n+1) / instances
		}
		out, err := single.Run(inputs[n])
		if err != nil {
			t.Fatal(err)
		}
		want[n] = append([]float32(nil), out...)
	}
	single.Close()
	if sameFloats(want[0], want[1], 1e-3) {
		t.Fatal("different inputs gave the same output, so a swapped result could not be seen")
	}

	backends := make([]*CoreMLBackend, instances)
	for n := range backends {
		backends[n], err = NewCoreMLBackend(compiled)
		if err != nil {
			t.Fatalf("NewCoreMLBackend: %v", err)
		}
		defer backends[n].Close()
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan string, instances)
	for n := range instances {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range runs {
				got, err := backends[n].Run(inputs[n])
				if err != nil {
					errs <- err.Error()
					return
				}
				if !sameFloats(got, want[n], 1e-3) {
					errs <- "a concurrent run returned a result that is not its own"
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Error(msg)
	}
}

// sameFloats reports whether two slices agree within tolerance. A NaN agrees
// with nothing.
func sameFloats(a, b []float32, tolerance float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !(math.Abs(float64(a[i])-float64(b[i])) <= tolerance) {
			return false
		}
	}
	return true
}
