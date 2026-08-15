package recording

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/media"
)

var (
	sharedBinOnce sync.Once
	sharedBin     string
	sharedBinErr  error
)

// sharedVedettaBinary builds the vedetta binary once for the subprocess tests.
// os.Executable() inside `go test` is the test binary, which has no `transcode`
// subcommand, so the out-of-process path must re-exec a real build.
func sharedVedettaBinary(t *testing.T) string {
	t.Helper()
	sharedBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "vedetta-bin")
		if err != nil {
			sharedBinErr = err
			return
		}
		bin := filepath.Join(dir, "vedetta")
		if out, err := exec.Command("go", "build", "-o", bin, "github.com/rvben/vedetta/cmd/vedetta").CombinedOutput(); err != nil {
			sharedBinErr = fmt.Errorf("build vedetta: %v\n%s", err, out)
			return
		}
		sharedBin = bin
	})
	if sharedBinErr != nil {
		t.Fatalf("%v", sharedBinErr)
	}
	return sharedBin
}

// useTestBinary points selfExecutable at the freshly built binary for the
// duration of a test.
func useTestBinary(t *testing.T) {
	t.Helper()
	bin := sharedVedettaBinary(t)
	prev := selfExecutable
	selfExecutable = func() (string, error) { return bin, nil }
	t.Cleanup(func() { selfExecutable = prev })
}

func ensureOpenH264OrSkip(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	status, err := media.InstallOpenH264(ctx)
	if err != nil {
		t.Skipf("OpenH264 unavailable (skipping, not a failure): %v", err)
	}
	if !status.Available {
		t.Skip("OpenH264 reported unavailable after install")
	}
}

// TestOutOfProcessTranscode_TranscodesFixture verifies the complete isolated
// worker, validation, and parent commit path on the committed fixture.
func TestOutOfProcessTranscode_TranscodesFixture(t *testing.T) {
	ensureOpenH264OrSkip(t)
	useTestBinary(t)

	fixture := filepath.Join("..", "media", "testdata", "sample_segment.mp4")
	src, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	oopPath := filepath.Join(t.TempDir(), "oop.mp4")
	if err := os.WriteFile(oopPath, src, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := outOfProcessTranscode(oopPath, 1280, 720)
	if err != nil {
		t.Fatalf("out-of-process transcode: %v", err)
	}

	if got.Skipped {
		t.Fatalf("out-of-process unexpectedly skipped fixture: %+v", got)
	}
	if got.OriginalSize != int64(len(src)) {
		t.Errorf("OriginalSize = %d, want %d", got.OriginalSize, len(src))
	}
	if got.NewSize <= 0 {
		t.Errorf("out-of-process produced non-positive NewSize: %d", got.NewSize)
	}

	// The parent atomically commits the validated stage; the resulting file size
	// must match the worker's reported NewSize.
	info, err := os.Stat(oopPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != got.NewSize {
		t.Errorf("file size %d != reported NewSize %d", info.Size(), got.NewSize)
	}
}

// TestOutOfProcessTranscode_BadInputReturnsError verifies a child that fails
// (here, unparseable input) surfaces as an error rather than a panic or a
// false success. This is the path that, for a heap-corruption crash, keeps the
// NVR alive by failing a single clip.
func TestOutOfProcessTranscode_BadInputReturnsError(t *testing.T) {
	useTestBinary(t)

	badPath := filepath.Join(t.TempDir(), "bad.mp4")
	if err := os.WriteFile(badPath, []byte("this is not an mp4 file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := outOfProcessTranscode(badPath, 1280, 720); err == nil {
		t.Fatal("expected an error for garbage input, got nil")
	}
}

func TestOutOfProcessTranscode_WorkerCrashPreservesSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test worker uses POSIX signals")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "recording.mp4")
	original := []byte("original recording bytes")
	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatal(err)
	}

	// This worker simulates the worst possible crash point: it destroys the
	// path it was given, leaves a partial temporary output, then segfaults.
	// The parent must only ever give it a disposable staging path.
	worker := filepath.Join(dir, "crashing-transcoder")
	script := "#!/bin/sh\ntarget=\"$6\"\nif [ \"$target\" = \"-output\" ]; then target=\"$7\"; fi\nprintf corrupt > \"$target\"\nprintf partial > \"$target.tmp\"\nkill -SEGV $$\n"
	if err := os.WriteFile(worker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	previousExecutable := selfExecutable
	selfExecutable = func() (string, error) { return worker, nil }
	t.Cleanup(func() { selfExecutable = previousExecutable })

	_, err := outOfProcessTranscode(path, 1280, 720)
	if err == nil {
		t.Fatal("crashing worker returned no error")
	}
	var workerErr *transcodeWorkerError
	if !errors.As(err, &workerErr) || workerErr.kind != transcodeFailureCrash {
		t.Fatalf("crash error = %v, want %q classification", err, transcodeFailureCrash)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read source after worker crash: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("source changed after worker crash: got %q, want %q", got, original)
	}

	leftovers, err := filepath.Glob(filepath.Join(dir, ".recording.mp4.recompress-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("worker crash left staging files: %v", leftovers)
	}
}

func TestOutOfProcessTranscode_InvalidWorkerOutputPreservesSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test worker uses a POSIX shell")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "recording.mp4")
	original := []byte("original recording bytes")
	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatal(err)
	}

	// A clean exit and syntactically valid result are not sufficient grounds
	// to replace a recording. The parent must independently validate the stage.
	worker := filepath.Join(dir, "lying-transcoder")
	script := "#!/bin/sh\nprintf corrupt > \"$7\"\nprintf '" + media.TranscodeResultMarker + "{\"original_size\":24,\"new_size\":7,\"skipped\":false}\\n'\n"
	if err := os.WriteFile(worker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	previousExecutable := selfExecutable
	selfExecutable = func() (string, error) { return worker, nil }
	t.Cleanup(func() { selfExecutable = previousExecutable })

	_, err := outOfProcessTranscode(path, 1280, 720)
	if err == nil {
		t.Fatal("worker with invalid output returned no error")
	}
	var workerErr *transcodeWorkerError
	if !errors.As(err, &workerErr) || workerErr.kind != transcodeFailureInvalidOutput {
		t.Fatalf("invalid-output error = %v, want %q classification", err, transcodeFailureInvalidOutput)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read source after rejected output: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("source changed after rejected output: got %q, want %q", got, original)
	}

	leftovers, err := filepath.Glob(filepath.Join(dir, ".recording.mp4.recompress-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("rejected output left staging files: %v", leftovers)
	}
}

func TestOutOfProcessTranscode_TimeoutPreservesSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test worker uses a POSIX shell")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "recording.mp4")
	original := []byte("original recording bytes")
	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatal(err)
	}

	worker := filepath.Join(dir, "wedged-transcoder")
	script := "#!/bin/sh\nprintf partial > \"$7.tmp\"\nwhile :; do :; done\n"
	if err := os.WriteFile(worker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	previousExecutable := selfExecutable
	selfExecutable = func() (string, error) { return worker, nil }
	t.Cleanup(func() { selfExecutable = previousExecutable })

	start := time.Now()
	_, err := outOfProcessTranscodeWithTimeout(path, 1280, 720, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout error = %v, want a clear timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("worker timeout took %s, want under 2s", elapsed)
	}
	var workerErr *transcodeWorkerError
	if !errors.As(err, &workerErr) || workerErr.kind != transcodeFailureTimeout {
		t.Fatalf("timeout error = %v, want %q classification", err, transcodeFailureTimeout)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read source after timeout: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("source changed after timeout: got %q, want %q", got, original)
	}

	leftovers, err := filepath.Glob(filepath.Join(dir, ".recording.mp4.recompress-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("timed-out worker left staging files: %v", leftovers)
	}
}

func TestBoundedTailBufferKeepsLatestBytes(t *testing.T) {
	buf := newBoundedTailBuffer(8)
	for _, chunk := range []string{"abc", "defgh", "ijkl"} {
		if n, err := buf.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = (%d, %v)", chunk, n, err)
		}
	}
	if got, want := buf.String(), "efghijkl"; got != want {
		t.Fatalf("buffer = %q, want latest bytes %q", got, want)
	}
	if len(buf.Bytes()) > 8 {
		t.Fatalf("buffer retained %d bytes, limit is 8", len(buf.Bytes()))
	}
}

func TestOutOfProcessTranscode_NonSmallerOutputIsSkipped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test worker uses a POSIX shell")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "recording.mp4")
	fixture := filepath.Join("..", "media", "testdata", "sample_segment.mp4")
	source, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, source, 0o640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// Copying the input produces a valid fMP4 and a plausible worker result,
	// but it reclaims no space and therefore must not replace the source.
	worker := filepath.Join(dir, "no-savings-transcoder")
	script := "#!/bin/sh\ncp \"$8\" \"$7\"\nsize=$(wc -c < \"$7\" | tr -d ' ')\nprintf '" + media.TranscodeResultMarker + "{\"original_size\":%s,\"new_size\":%s,\"skipped\":false}\\n' \"$size\" \"$size\"\n"
	if err := os.WriteFile(worker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	previousExecutable := selfExecutable
	selfExecutable = func() (string, error) { return worker, nil }
	t.Cleanup(func() { selfExecutable = previousExecutable })

	result, err := outOfProcessTranscode(path, 1280, 720)
	if err != nil {
		t.Fatalf("non-smaller output: %v", err)
	}
	if !result.Skipped {
		t.Fatalf("result = %+v, want skipped", result)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("non-smaller output replaced the source file")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, source) {
		t.Fatal("non-smaller output changed the source bytes")
	}
}

func TestOutOfProcessTranscode_SourceReplacementAbortsCommit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test worker uses a POSIX shell")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "recording.mp4")
	fixturePath := filepath.Join("..", "media", "testdata", "sample_segment.mp4")
	fixture, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	original := append(append([]byte(nil), fixture...), bytes.Repeat([]byte{'O'}, 4096)...)
	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatal(err)
	}
	stageFixture := filepath.Join(dir, "valid-stage.mp4")
	if err := os.WriteFile(stageFixture, fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	replacement := bytes.Repeat([]byte{'R'}, len(original))
	replacementPath := filepath.Join(dir, "external-replacement.mp4")
	if err := os.WriteFile(replacementPath, replacement, 0o600); err != nil {
		t.Fatal(err)
	}

	// Simulate another process atomically replacing the source after the worker
	// has produced a valid, smaller stage but before the parent commits it.
	worker := filepath.Join(dir, "source-replacing-transcoder")
	script := "#!/bin/sh\noriginal=$(wc -c < \"$8\" | tr -d ' ')\ncp \"$(dirname \"$0\")/valid-stage.mp4\" \"$7\"\nnew=$(wc -c < \"$7\" | tr -d ' ')\nmv \"$(dirname \"$0\")/external-replacement.mp4\" \"$8\"\nprintf '" + media.TranscodeResultMarker + "{\"original_size\":%s,\"new_size\":%s,\"skipped\":false}\\n' \"$original\" \"$new\"\n"
	if err := os.WriteFile(worker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	previousExecutable := selfExecutable
	selfExecutable = func() (string, error) { return worker, nil }
	t.Cleanup(func() { selfExecutable = previousExecutable })

	_, err = outOfProcessTranscode(path, 1280, 720)
	if err == nil {
		t.Fatal("source replacement was overwritten; want commit rejection")
	}
	var workerErr *transcodeWorkerError
	if !errors.As(err, &workerErr) || workerErr.kind != transcodeFailureSourceChanged {
		t.Fatalf("source-change error = %v, want %q classification", err, transcodeFailureSourceChanged)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, replacement) {
		t.Fatal("source replacement did not survive rejected transcode commit")
	}
}

func TestParseTranscodeResult(t *testing.T) {
	t.Run("marker among noise", func(t *testing.T) {
		stdout := "[OpenH264] this = 0x123, Warning: blah\n" +
			media.TranscodeResultMarker + `{"original_size":100,"new_size":40,"skipped":false}` + "\n" +
			"trailing noise\n"
		res, err := parseTranscodeResult([]byte(stdout))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if res.OriginalSize != 100 || res.NewSize != 40 || res.Skipped {
			t.Errorf("unexpected result: %+v", res)
		}
	})

	t.Run("skipped result", func(t *testing.T) {
		res, err := parseTranscodeResult([]byte(media.TranscodeResultMarker + `{"skipped":true}` + "\n"))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if !res.Skipped {
			t.Errorf("expected Skipped=true, got %+v", res)
		}
	})

	t.Run("no marker is an error", func(t *testing.T) {
		if _, err := parseTranscodeResult([]byte("no result line here\n")); err == nil {
			t.Error("expected error when marker absent")
		}
	})
}
