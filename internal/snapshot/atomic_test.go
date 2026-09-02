package snapshot

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// noiseImage returns an image whose pixels do not compress away, so the encoder
// produces enough output to be written in several buffers rather than one.
func noiseImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			v := uint8((x*7 + y*13) % 256)
			img.Set(x, y, color.RGBA{R: v, G: v ^ 0x5a, B: v + 97, A: 255})
		}
	}
	return img
}

// TestSaveSnapshotNeverExposesAPartialFile is the case a truncating write used
// to lose: the camera overwrites the same snapshot path on every capture, and
// anything reading that path while the encoder is running (the API serving a
// thumbnail, the notification dispatcher attaching one) sees an empty or
// half-written JPEG rather than the previous capture.
//
// The reader can only ever observe a complete file, so this test cannot fail on
// a correct implementation. Against a write that truncates the destination it
// fails immediately.
func TestSaveSnapshotNeverExposesAPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snap.jpg")

	if err := SaveSnapshot(noiseImage(32, 32), path, 85); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	var reads, partial atomic.Int64
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			data, err := os.ReadFile(path)
			if err != nil {
				partial.Add(1)
				continue
			}
			reads.Add(1)
			if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
				partial.Add(1)
			}
		}
	}()

	if err := SaveSnapshot(noiseImage(1920, 1080), path, 90); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	close(stop)
	<-done

	// Positive control: a reader that never saw the file would report zero
	// partial reads for the wrong reason.
	if reads.Load() == 0 {
		t.Fatal("the reader never read the snapshot, so it could not have observed a partial one")
	}
	if n := partial.Load(); n > 0 {
		t.Fatalf("a reader observed %d unreadable snapshots across %d reads, so the destination held a partial file", n, reads.Load())
	}
}

// TestSaveSnapshotLeavesNoTemporaryFile keeps the atomic write from trading one
// defect for a directory that fills with debris.
func TestSaveSnapshotLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snap.jpg")

	if err := SaveSnapshot(noiseImage(64, 64), path, 85); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "snap.jpg" {
		t.Fatalf("directory holds %v, want only snap.jpg", names)
	}
}

// TestSaveSnapshotKeepsThePreviousCaptureWhenTheTargetCannotBeReplaced proves
// the failure path preserves what was already on disk instead of leaving the
// caller with nothing.
func TestSaveSnapshotKeepsThePreviousCaptureWhenTheTargetCannotBeReplaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snap.jpg")

	if err := SaveSnapshot(noiseImage(32, 32), path, 85); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}

	// A directory at the destination cannot be replaced by a rename, so the
	// write fails after the encode succeeded.
	blocked := filepath.Join(dir, "blocked.jpg")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := SaveSnapshot(noiseImage(32, 32), blocked, 85); err == nil {
		t.Fatal("saving over a directory succeeded, want an error")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after failure: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the previous capture changed while an unrelated save failed")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("directory holds %d entries after a failed save, want 2 (the snapshot and the blocking directory)", len(entries))
	}
}

// A snapshot is replaced rather than written in place, so its mode is chosen by
// this code instead of inherited. A fixed mode overrides the operator: on a
// host whose umask is deliberately strict, every capture republishes the image
// wider than the operator asked for. The control file is created with os.Create
// in the same directory, which is exactly what the mode has to match.
func TestSaveSnapshotMatchesAPlainCreateForANewFile(t *testing.T) {
	dir := t.TempDir()

	control, err := os.Create(filepath.Join(dir, "control.jpg"))
	if err != nil {
		t.Fatalf("create control file: %v", err)
	}
	if err := control.Close(); err != nil {
		t.Fatalf("close control file: %v", err)
	}
	controlInfo, err := os.Stat(control.Name())
	if err != nil {
		t.Fatalf("stat control file: %v", err)
	}

	path := filepath.Join(dir, "snap.jpg")
	if err := SaveSnapshot(noiseImage(16, 16), path, 85); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat snapshot: %v", err)
	}

	if info.Mode().Perm() != controlInfo.Mode().Perm() {
		t.Fatalf("new snapshot is %v, a plain create here produces %v",
			info.Mode().Perm(), controlInfo.Mode().Perm())
	}
}

// The durable half of the same rule: an operator who tightened an existing
// snapshot must not have the next capture, seconds later, widen it again.
func TestSaveSnapshotKeepsTheModeOfTheFileItReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snap.jpg")

	if err := SaveSnapshot(noiseImage(16, 16), path, 85); err != nil {
		t.Fatalf("first SaveSnapshot: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if err := SaveSnapshot(noiseImage(16, 16), path, 85); err != nil {
		t.Fatalf("second SaveSnapshot: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat snapshot: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("replacing the snapshot changed its mode to %v, want 0600", info.Mode().Perm())
	}
}
