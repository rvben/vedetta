package media

// Developer-only heap-corruption reproduction harness for the recompression
// transcode path. NOT run in CI: skips unless VEDETTA_CORPUS_DIR is set.
//
// It transcodes a corpus of real recordings many times in one process while a
// canary goroutine churns small allocations and channel operations (sudogs),
// so a latent out-of-bounds write planted by the encoder hits a live victim
// object and surfaces as a fatal runtime error (or a recovered Pinner panic)
// far sooner than the once-per-few-hours rate seen in production.
//
//   VEDETTA_CORPUS_DIR=~/vedetta-repro-clips \
//   VEDETTA_STRESS_ITERS=50 \
//   GODEBUG=clobberfree=1 GOGC=10 \
//   go test ./internal/media/ -run TestTranscodeCorpusStress -count=1 -v -timeout=2h

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

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// canary keeps the heap populated with churning small objects and sudogs so
// that an out-of-bounds write into Go-managed memory is likely to land on a
// live, soon-to-be-validated object and trip a fatal runtime check.
func canary(stop <-chan struct{}, ops *uint64) {
	ch := make(chan []byte, 8)
	// producer/consumer pair exercises the sudog freelist that the production
	// crash (acquireSudog: found s.elem != nil) corrupts.
	go func() {
		for {
			select {
			case <-stop:
				return
			case b := <-ch:
				if len(b) > 0 {
					b[0]++
					b[len(b)-1]++
				}
			}
		}
	}()
	m := make(map[int][]byte, 4096)
	i := 0
	for {
		select {
		case <-stop:
			return
		default:
		}
		buf := make([]byte, 32+(i%512))
		for j := range buf {
			buf[j] = byte(i + j)
		}
		m[i%4096] = buf
		select {
		case ch <- buf:
		default:
		}
		i++
		atomic.AddUint64(ops, 1)
		if i%4096 == 0 {
			runtime.GC()
		}
	}
}

func TestTranscodeCorpusStress(t *testing.T) {
	dir := os.Getenv("VEDETTA_CORPUS_DIR")
	if dir == "" {
		t.Skip("set VEDETTA_CORPUS_DIR to a directory of real recordings")
	}
	if !ensureOpenH264() {
		t.Skip("OpenH264 not available")
	}

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
	iters := envInt("VEDETTA_STRESS_ITERS", 50)
	workers := envInt("VEDETTA_STRESS_WORKERS", 1)
	t.Logf("corpus: %d clips, %d iterations, %d workers, GOMAXPROCS=%d", len(clips), iters, workers, runtime.GOMAXPROCS(0))

	stop := make(chan struct{})
	var canaryOps uint64
	for i := 0; i < 3; i++ {
		go canary(stop, &canaryOps)
	}
	defer close(stop)

	start := time.Now()
	var transcodes, recovered, skipped, failed uint64

	runOne := func(clip string) {
		src, err := os.ReadFile(clip)
		if err != nil {
			t.Errorf("read %s: %v", clip, err)
			return
		}
		tmp, err := os.CreateTemp("", "stress_*.mp4")
		if err != nil {
			t.Errorf("temp: %v", err)
			return
		}
		tmpPath := tmp.Name()
		_, _ = tmp.Write(src)
		tmp.Close()
		defer os.Remove(tmpPath)

		defer func() {
			if p := recover(); p != nil {
				atomic.AddUint64(&recovered, 1)
				t.Errorf("RECOVERED panic transcoding %s: %v", clip, p)
			}
		}()
		res, err := TranscodeSegment(tmpPath, 1280, 720)
		switch {
		case err != nil:
			atomic.AddUint64(&failed, 1)
		case res.Skipped:
			atomic.AddUint64(&skipped, 1)
		default:
			atomic.AddUint64(&transcodes, 1)
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
					t.Logf("iter %d/%d: %d ok, %d skipped, %d failed, %d recovered-panics, canaryOps=%d, elapsed=%s",
						it+1, iters, atomic.LoadUint64(&transcodes), atomic.LoadUint64(&skipped),
						atomic.LoadUint64(&failed), atomic.LoadUint64(&recovered),
						atomic.LoadUint64(&canaryOps), time.Since(start).Round(time.Second))
				}
			}
		}(w)
	}
	wg.Wait()
	t.Logf("DONE: %d transcodes, %d skipped, %d failed, %d recovered panics in %s",
		atomic.LoadUint64(&transcodes), atomic.LoadUint64(&skipped),
		atomic.LoadUint64(&failed), atomic.LoadUint64(&recovered), time.Since(start).Round(time.Second))
}
