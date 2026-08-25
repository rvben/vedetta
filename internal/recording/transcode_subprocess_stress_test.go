package recording

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func envIntRec(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// TestOutOfProcessTranscodeStress is the survival counterpart to the in-process
// media stress harness. It drives the same real-clip corpus through
// outOfProcessTranscode with concurrent workers. The in-process transcode path
// corrupts the test process heap and crashes it within ~30s under this load;
// the out-of-process path must keep THIS process alive no matter how many child
// transcodes crash. Dev-only: skips unless VEDETTA_CORPUS_DIR is set.
//
//	VEDETTA_CORPUS_DIR=~/vedetta-repro-clips VEDETTA_STRESS_ITERS=8 VEDETTA_STRESS_WORKERS=6 \
//	  go test ./internal/recording/ -run TestOutOfProcessTranscodeStress -count=1 -v -timeout=2h
func TestOutOfProcessTranscodeStress(t *testing.T) {
	dir := os.Getenv("VEDETTA_CORPUS_DIR")
	if dir == "" {
		t.Skip("set VEDETTA_CORPUS_DIR to a directory of real recordings")
	}
	ensureOpenH264OrSkip(t) // children need OpenH264 to transcode
	useTestBinary(t)

	var clips []string
	if err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Ext(p) == ".mp4" {
			clips = append(clips, p)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk corpus: %v", err)
	}
	if len(clips) == 0 {
		t.Fatalf("no .mp4 clips under %s", dir)
	}

	iters := envIntRec("VEDETTA_STRESS_ITERS", 8)
	workers := envIntRec("VEDETTA_STRESS_WORKERS", 6)
	t.Logf("corpus: %d clips, %d iterations, %d workers", len(clips), iters, workers)

	start := time.Now()
	var ok, failed uint64

	runOne := func(clip string) {
		src, err := os.ReadFile(clip)
		if err != nil {
			t.Errorf("read %s: %v", clip, err)
			return
		}
		tmp, err := os.CreateTemp("", "oopstress_*.mp4")
		if err != nil {
			t.Errorf("temp: %v", err)
			return
		}
		tmpPath := tmp.Name()
		_, _ = tmp.Write(src)
		tmp.Close()
		defer os.Remove(tmpPath)

		if _, err := outOfProcessTranscode(tmpPath, 1280, 720); err != nil {
			// A crashed/failed child is expected and tolerated: the point is
			// that it never takes down this parent process.
			atomic.AddUint64(&failed, 1)
		} else {
			atomic.AddUint64(&ok, 1)
		}
	}

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for it := 0; it < iters; it++ {
				for _, clip := range clips {
					runOne(clip)
				}
				if w == 0 {
					t.Logf("iter %d/%d: %d ok, %d child-failures, goroutines=%d, elapsed=%s",
						it+1, iters, atomic.LoadUint64(&ok), atomic.LoadUint64(&failed),
						runtime.NumGoroutine(), time.Since(start).Round(time.Second))
				}
			}
		}(w)
	}
	wg.Wait()
	t.Logf("DONE: parent survived %d transcodes (%d ok, %d child-failures) in %s",
		ok+failed, ok, failed, time.Since(start).Round(time.Second))
}
