package camera

import (
	"image"
	"image/color"
	"os"
	"path/filepath"
	"testing"
)

// testFrame returns a small frame the camera can encode.
func testFrame(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0x40, A: 255})
		}
	}
	return img
}

// blockedSnapshotCamera returns a camera whose cached-snapshot destination
// cannot be written, because a directory occupies the path. A rename onto a
// directory fails, which is the failure this camera has to survive without
// losing state or leaving debris.
func blockedSnapshotCamera(t *testing.T) (*Camera, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "snap.jpg")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cam := NewTestCamera("front")
	cam.SetTestFrame(testFrame(48, 32))
	cam.latestSnapshotPath = path
	return cam, dir
}

// TestSaveCachedSnapshotKeepsRetryingAfterAFailedWrite is the case an unchecked
// rename used to hide: the write fails, nothing reports it, and the throttle
// timestamp advances anyway, so the camera waits a full interval before trying
// again and the on-disk snapshot silently ages.
func TestSaveCachedSnapshotKeepsRetryingAfterAFailedWrite(t *testing.T) {
	cam, _ := blockedSnapshotCamera(t)

	cam.saveCachedSnapshot()

	cam.mu.RLock()
	saved := cam.lastSnapshotSave
	cam.mu.RUnlock()
	if !saved.IsZero() {
		t.Fatalf("the throttle advanced to %v after a failed write, so the next capture is suppressed", saved)
	}
}

// TestSaveCachedSnapshotLeavesNoTemporaryFile keeps a failed write from
// accumulating debris beside the snapshot it could not replace.
func TestSaveCachedSnapshotLeavesNoTemporaryFile(t *testing.T) {
	cam, dir := blockedSnapshotCamera(t)

	cam.saveCachedSnapshot()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "snap.jpg" {
		t.Fatalf("directory holds %v after a failed write, want only the blocked snapshot path", names)
	}
}

// TestSaveCachedSnapshotWritesTheFrameAndThrottles is the positive control for
// the two failure tests: without it, a saveCachedSnapshot that did nothing at
// all would pass both.
func TestSaveCachedSnapshotWritesTheFrameAndThrottles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snap.jpg")
	cam := NewTestCamera("front")
	cam.SetTestFrame(testFrame(48, 32))
	cam.latestSnapshotPath = path

	cam.saveCachedSnapshot()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat snapshot: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("the snapshot is empty")
	}
	cam.mu.RLock()
	saved := cam.lastSnapshotSave
	cam.mu.RUnlock()
	if saved.IsZero() {
		t.Fatal("a successful write did not advance the throttle")
	}
}
