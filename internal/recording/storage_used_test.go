package recording

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/config"
)

func writeDummyClip(t *testing.T, root, cam, name string, size int64) {
	t.Helper()
	dir := filepath.Join(root, cam, "clips", "2026-01-02")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The dashboard and Settings read StorageStats while the Storage page reads
// StorageBreakdown. Both must report the same used figure, clips included.
func TestStorageStatsUsedBytesMatchesBreakdown(t *testing.T) {
	tmp := t.TempDir()
	db := openTestStorageDB(t)
	cfg := newTestRecConfig(tmp)
	r := New(cfg, config.EventConfig{RetainDays: 90}, nil, db, nil, "", "", nil)
	r.RegisterCamera("cam-a", "rtsp://192.0.2.10/a")
	r.RegisterCamera("cam-b", "rtsp://192.0.2.11/b")

	when := time.Now().UTC().Add(-time.Hour)
	seg := writeDummySegment(t, cfg.Path, "cam-a", when, 4000)
	mustSaveSegmentRecord(t, db, "cam-a", seg, when, 4000)
	writeDummyClip(t, cfg.Path, "cam-a", "one.mp4", 700)
	writeDummyClip(t, cfg.Path, "cam-b", "two.mp4", 300)

	r.RefreshStats()
	stats := r.StorageStats()

	if stats.TotalBytes != 4000 {
		t.Fatalf("TotalBytes = %d, want the 4000 segment bytes", stats.TotalBytes)
	}
	if stats.ClipBytes != 1000 {
		t.Errorf("ClipBytes = %d, want 1000", stats.ClipBytes)
	}
	if got := stats.CameraClipBytes["cam-a"]; got != 700 {
		t.Errorf("CameraClipBytes[cam-a] = %d, want 700", got)
	}
	if got := stats.CameraClipBytes["cam-b"]; got != 300 {
		t.Errorf("CameraClipBytes[cam-b] = %d, want 300", got)
	}
	if got := stats.UsedBytes(); got != 5000 {
		t.Errorf("UsedBytes() = %d, want 5000 (segments plus clips)", got)
	}

	breakdown, err := r.StorageBreakdown()
	if err != nil {
		t.Fatal(err)
	}
	if breakdown.Recording.UsedBytes != stats.UsedBytes() {
		t.Errorf("breakdown used = %d, stats used = %d; the pages would disagree",
			breakdown.Recording.UsedBytes, stats.UsedBytes())
	}
}
