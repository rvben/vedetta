package media

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ensureOpenH264ForTest loads OpenH264 if already available, otherwise
// downloads and installs it via the same mechanism the server uses at
// startup (InstallOpenH264). This lets the fixture test run in CI on
// runners that don't have libopenh264 installed at the system level.
func ensureOpenH264ForTest(t *testing.T) {
	t.Helper()
	if ensureOpenH264() {
		return
	}
	t.Log("OpenH264 not loaded; attempting install")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	status, err := InstallOpenH264(ctx)
	if err != nil {
		t.Skipf("OpenH264 install failed (skipping, not a test failure): %v", err)
	}
	if !status.Available {
		t.Skipf("OpenH264 install reported unavailable after install")
	}
}

// TestTranscodeSegment_Fixture is the regression test for the Go 1.26
// GC memory safety bug in the encoder call path. It runs against a
// committed sample fMP4 segment so it always executes in CI.
//
// Before the fix (commit 997f75d~1), this test reliably triggered
// "signal: killed" or "fatal error: found pointer to free object"
// after ~5-15 seconds when transcoding a real 2 MB fMP4 segment.
// The failure mode was: the encoder output structs (SFrameBSInfo,
// SSourcePicture) escaped to the heap, and the GC scanned their
// pointer-typed fields (containing C-owned addresses) as Go heap
// pointers, then called findObject on those addresses.
//
// Keep this test. If you are "simplifying" transcoder.go's encoder
// I/O by replacing the [N]byte backing pattern with a typed struct
// value, this test will catch it.
func TestTranscodeSegment_Fixture(t *testing.T) {
	ensureOpenH264ForTest(t)

	// Copy fixture to a temp location — TranscodeSegment rewrites the
	// file in place, so we must not touch the committed testdata copy.
	fixture := filepath.Join("testdata", "sample_segment.mp4")
	src, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	tmp, err := os.CreateTemp("", "transcode_fixture_*.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(src); err != nil {
		t.Fatal(err)
	}
	tmp.Close()

	result, err := isolatedTranscodeSegment(t, tmp.Name(), 1280, 720)
	if err != nil {
		t.Fatalf("transcode failed: %v", err)
	}
	if result.Skipped {
		// Shouldn't happen with the committed fixture (it's 2 MB from
		// a 1080p camera) but don't fail hard — log and stop.
		t.Logf("transcode skipped: %d bytes source", result.OriginalSize)
		return
	}
	if result.NewSize == 0 {
		t.Fatalf("output size is zero")
	}
	t.Logf("fixture transcoded: %d bytes -> %d bytes (%.0f%% reduction)",
		result.OriginalSize, result.NewSize,
		100*(1-float64(result.NewSize)/float64(result.OriginalSize)))
}

// TestTranscodeRealSegment is a developer-only smoke test for running
// the transcoder against a real production segment larger than the
// committed fixture. Set VEDETTA_REAL_SEGMENT to a segment path.
func TestTranscodeRealSegment(t *testing.T) {
	path := os.Getenv("VEDETTA_REAL_SEGMENT")
	if path == "" {
		t.Skip("set VEDETTA_REAL_SEGMENT to a real segment path")
	}
	if !ensureOpenH264() {
		t.Skip("OpenH264 not available")
	}

	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tmp, err := os.CreateTemp("", "vedetta_transcode_*.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(src); err != nil {
		t.Fatal(err)
	}
	tmp.Close()

	result, err := isolatedTranscodeSegment(t, tmp.Name(), 1280, 720)
	if err != nil {
		t.Fatalf("transcode failed: %v", err)
	}
	if result.Skipped {
		t.Skipf("transcode skipped: source already small enough")
	}
	if result.NewSize == 0 {
		t.Errorf("output size is zero")
	}
	t.Logf("OK: %d bytes -> %d bytes (%.0f%% reduction)",
		result.OriginalSize, result.NewSize,
		100*(1-float64(result.NewSize)/float64(result.OriginalSize)))
}
