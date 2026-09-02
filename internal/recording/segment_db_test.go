package recording

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/storage"
)

func TestSaveSegmentAndQueryRoundtrip(t *testing.T) {
	sr := newTestSegmentRecorder(t)
	now := time.Now().Truncate(time.Second)

	seg := storage.SegmentRecord{
		Camera:    "cam1",
		Path:      "/recordings/cam1/segments/2026-01-01_12-00-00.mp4",
		StartTime: now.Add(-10 * time.Minute),
		EndTime:   now,
		SizeBytes: 1024000,
	}

	if err := sr.db.SaveSegment(seg); err != nil {
		t.Fatalf("SaveSegment failed: %v", err)
	}

	// Query a range that overlaps the segment
	results, err := sr.db.QuerySegments("cam1", now.Add(-5*time.Minute), now.Add(1*time.Minute))
	if err != nil {
		t.Fatalf("QuerySegments failed: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(results))
	}

	got := results[0]
	if got.Camera != seg.Camera {
		t.Errorf("camera mismatch: got %q, want %q", got.Camera, seg.Camera)
	}
	if got.Path != seg.Path {
		t.Errorf("path mismatch: got %q, want %q", got.Path, seg.Path)
	}
	if got.SizeBytes != seg.SizeBytes {
		t.Errorf("size mismatch: got %d, want %d", got.SizeBytes, seg.SizeBytes)
	}
	if !got.StartTime.Equal(seg.StartTime) {
		t.Errorf("start_time mismatch: got %v, want %v", got.StartTime, seg.StartTime)
	}
	if !got.EndTime.Equal(seg.EndTime) {
		t.Errorf("end_time mismatch: got %v, want %v", got.EndTime, seg.EndTime)
	}

	// Query a range that does not overlap
	noResults, err := sr.db.QuerySegments("cam1", now.Add(1*time.Hour), now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("QuerySegments failed: %v", err)
	}
	if len(noResults) != 0 {
		t.Errorf("expected 0 segments for non-overlapping range, got %d", len(noResults))
	}
}

func TestDeleteSegment(t *testing.T) {
	sr := newTestSegmentRecorder(t)
	now := time.Now().Truncate(time.Second)

	seg := storage.SegmentRecord{
		Camera:    "cam1",
		Path:      "/recordings/cam1/segments/test.mp4",
		StartTime: now.Add(-10 * time.Minute),
		EndTime:   now,
	}
	if err := sr.db.SaveSegment(seg); err != nil {
		t.Fatalf("SaveSegment failed: %v", err)
	}

	if err := sr.db.DeleteSegment(seg.Path); err != nil {
		t.Fatalf("DeleteSegment failed: %v", err)
	}

	all, err := sr.db.GetAllSegments("cam1")
	if err != nil {
		t.Fatalf("GetAllSegments failed: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("expected 0 segments after deletion, got %d", len(all))
	}
}

func TestGetAllSegments(t *testing.T) {
	sr := newTestSegmentRecorder(t)
	now := time.Now().Truncate(time.Second)

	seedSegments(t, sr, []storage.SegmentRecord{
		{Camera: "cam1", Path: "/a.mp4", StartTime: now.Add(-30 * time.Minute), EndTime: now.Add(-20 * time.Minute)},
		{Camera: "cam1", Path: "/b.mp4", StartTime: now.Add(-20 * time.Minute), EndTime: now.Add(-10 * time.Minute)},
		{Camera: "cam2", Path: "/c.mp4", StartTime: now.Add(-10 * time.Minute), EndTime: now},
	})

	cam1Segs, err := sr.db.GetAllSegments("cam1")
	if err != nil {
		t.Fatalf("GetAllSegments failed: %v", err)
	}
	if len(cam1Segs) != 2 {
		t.Errorf("expected 2 segments for cam1, got %d", len(cam1Segs))
	}

	cam2Segs, err := sr.db.GetAllSegments("cam2")
	if err != nil {
		t.Fatalf("GetAllSegments failed: %v", err)
	}
	if len(cam2Segs) != 1 {
		t.Errorf("expected 1 segment for cam2, got %d", len(cam2Segs))
	}
}

func TestGetOldestSegments(t *testing.T) {
	sr := newTestSegmentRecorder(t)
	now := time.Now().Truncate(time.Second)

	seedSegments(t, sr, []storage.SegmentRecord{
		{Camera: "cam1", Path: "/c.mp4", StartTime: now.Add(-10 * time.Minute), EndTime: now, SizeBytes: 300},
		{Camera: "cam2", Path: "/a.mp4", StartTime: now.Add(-30 * time.Minute), EndTime: now.Add(-20 * time.Minute), SizeBytes: 100},
		{Camera: "cam1", Path: "/b.mp4", StartTime: now.Add(-20 * time.Minute), EndTime: now.Add(-10 * time.Minute), SizeBytes: 200},
	})

	oldest, err := sr.db.GetOldestSegments(2)
	if err != nil {
		t.Fatalf("GetOldestSegments: %v", err)
	}
	if len(oldest) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(oldest))
	}
	// Should be ordered by start_time ascending, across all cameras
	if oldest[0].Path != "/a.mp4" {
		t.Errorf("expected oldest segment /a.mp4, got %s", oldest[0].Path)
	}
	if oldest[1].Path != "/b.mp4" {
		t.Errorf("expected second oldest /b.mp4, got %s", oldest[1].Path)
	}
}

func TestGetOldestSegments_Empty(t *testing.T) {
	sr := newTestSegmentRecorder(t)

	oldest, err := sr.db.GetOldestSegments(10)
	if err != nil {
		t.Fatalf("GetOldestSegments: %v", err)
	}
	if len(oldest) != 0 {
		t.Errorf("expected 0 segments, got %d", len(oldest))
	}
}

func TestScanExistingSegments_FindsNewFiles(t *testing.T) {
	sr := newTestSegmentRecorder(t)
	segDir := filepath.Join(t.TempDir(), "cam1", "segments")
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// A real segment: the scan has to read its duration to place it on the
	// timeline, so a stand-in byte string cannot exercise the import.
	segPath := filepath.Join(segDir, "2026-01-01_12-00-00.mp4")
	writeProbeableSegment(t, segPath)

	sr.ScanExistingSegments("cam1", segDir)

	all, err := sr.db.GetAllSegments("cam1")
	if err != nil {
		t.Fatalf("GetAllSegments failed: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 segment after scan, got %d", len(all))
	}
	if all[0].Path != segPath {
		t.Errorf("expected path %q, got %q", segPath, all[0].Path)
	}
	if d := all[0].EndTime.Sub(all[0].StartTime); d <= 0 {
		t.Errorf("imported segment spans %v, want the duration read from the file", d)
	}
}

func TestScanExistingSegments_RemovesOrphanedRecords(t *testing.T) {
	sr := newTestSegmentRecorder(t)
	segDir := filepath.Join(t.TempDir(), "cam1", "segments")
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		t.Fatal(err)
	}

	now := time.Now().Truncate(time.Second)

	// Insert a DB record for a file that does not exist on disk
	orphanSeg := storage.SegmentRecord{
		Camera:    "cam1",
		Path:      filepath.Join(segDir, "nonexistent.mp4"),
		StartTime: now.Add(-10 * time.Minute),
		EndTime:   now,
	}
	if err := sr.db.SaveSegment(orphanSeg); err != nil {
		t.Fatalf("SaveSegment failed: %v", err)
	}

	sr.ScanExistingSegments("cam1", segDir)

	all, err := sr.db.GetAllSegments("cam1")
	if err != nil {
		t.Fatalf("GetAllSegments failed: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("expected 0 segments after orphan cleanup, got %d", len(all))
	}
}

func TestScanExistingSegments_SkipsAlreadyKnown(t *testing.T) {
	sr := newTestSegmentRecorder(t)
	segDir := filepath.Join(t.TempDir(), "cam1", "segments")
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		t.Fatal(err)
	}

	now := time.Now().Truncate(time.Second)

	// Create file on disk
	segPath := filepath.Join(segDir, "existing.mp4")
	if err := os.WriteFile(segPath, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Insert matching DB record
	rec := storage.SegmentRecord{
		Camera:    "cam1",
		Path:      segPath,
		StartTime: now.Add(-10 * time.Minute),
		EndTime:   now,
		SizeBytes: 4,
	}
	if err := sr.db.SaveSegment(rec); err != nil {
		t.Fatal(err)
	}

	sr.ScanExistingSegments("cam1", segDir)

	all, err := sr.db.GetAllSegments("cam1")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Errorf("expected 1 segment (unchanged), got %d", len(all))
	}
	// The original start time should be preserved (not overwritten by scan)
	if !all[0].StartTime.Equal(rec.StartTime) {
		t.Errorf("start time changed after re-scan: got %v, want %v", all[0].StartTime, rec.StartTime)
	}
}
