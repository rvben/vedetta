package storage

// These tests verify that all timestamp-based queries return correct results.
// The queries use replace(col, 'T', ' ') normalization to handle both
// Go String() format ("2006-01-02 15:04:05 +0000 UTC") found in older
// production databases and RFC3339 ("2006-01-02T15:04:05Z") used by the
// current SQLite driver. The modernc.org/sqlite driver normalizes timestamp
// strings to RFC3339 on write, so these tests exercise the RFC3339 path.
// The replace() in queries provides defense-in-depth for legacy data.

import (
	"testing"
	"time"
)

// insertSegmentRaw inserts a segment using the Go String() timestamp format
// that production databases use, bypassing the Go SQL driver's serialization.
func insertSegmentRaw(t *testing.T, db *DB, cam, path string, start, end time.Time, size int64) {
	t.Helper()
	const goFmt = "2006-01-02 15:04:05.999999 +0000 UTC"
	_, err := db.db.Exec(
		`INSERT INTO segments (camera, path, start_time, end_time, size_bytes) VALUES (?, ?, ?, ?, ?)`,
		cam, path, start.UTC().Format(goFmt), end.UTC().Format(goFmt), size,
	)
	if err != nil {
		t.Fatalf("insertSegmentRaw: %v", err)
	}
}

// insertMotionRaw inserts motion activity using the Go String() format.
func insertMotionRaw(t *testing.T, db *DB, cam string, bucket time.Time, score float64) {
	t.Helper()
	const goFmt = "2006-01-02 15:04:05 +0000 UTC"
	_, err := db.db.Exec(
		`INSERT OR REPLACE INTO motion_activity (camera, bucket, score) VALUES (?, ?, ?)`,
		cam, bucket.UTC().Format(goFmt), score,
	)
	if err != nil {
		t.Fatalf("insertMotionRaw: %v", err)
	}
}

// insertEventRaw inserts an event using the Go String() format.
func insertEventRaw(t *testing.T, db *DB, id, cam, label string, ts time.Time) {
	t.Helper()
	const goFmt = "2006-01-02 15:04:05.999999 +0000 UTC"
	_, err := db.db.Exec(
		`INSERT INTO events (id, camera, label, score, box_x1, box_y1, box_x2, box_y2, timestamp)
		 VALUES (?, ?, ?, 0.9, 0, 0, 100, 100, ?)`,
		id, cam, label, ts.UTC().Format(goFmt),
	)
	if err != nil {
		t.Fatalf("insertEventRaw: %v", err)
	}
}

func TestQuerySegments_GoStringFormat(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()
	start := now.Add(-10 * time.Minute)
	end := now

	insertSegmentRaw(t, db, "cam1", "/seg.mp4", start, end, 1000)

	// Query for a time range that overlaps the segment
	segs, err := db.QuerySegments("cam1", start.Add(5*time.Minute), end.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 1 {
		t.Errorf("QuerySegments: got %d segments, want 1", len(segs))
	}
}

func TestGetSegmentsOverlapping_GoStringFormat(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()

	insertSegmentRaw(t, db, "cam1", "/seg1.mp4", now.Add(-2*time.Hour), now.Add(-1*time.Hour), 1000)
	insertSegmentRaw(t, db, "cam1", "/seg2.mp4", now.Add(-1*time.Hour), now, 2000)

	segs, err := db.GetSegmentsOverlapping("cam1", now.Add(-24*time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 {
		t.Errorf("GetSegmentsOverlapping: got %d segments, want 2", len(segs))
	}
}

func TestGetRecordingDays_GoStringFormat(t *testing.T) {
	db := newTestDB(t)
	// Fixed mid-day instant: the test exercises raw timestamp-format
	// compatibility, not day-boundary logic.
	ts := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)

	insertSegmentRaw(t, db, "cam1", "/seg.mp4", ts.Add(-1*time.Hour), ts, 1000)

	days, err := db.GetRecordingDays("cam1", ts.Year(), int(ts.Month()), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 1 {
		t.Errorf("GetRecordingDays: got %d days, want 1", len(days))
	}
	if len(days) > 0 && days[0] != ts.Day() {
		t.Errorf("GetRecordingDays: got day %d, want %d", days[0], ts.Day())
	}
}

func TestGetMotionActivityInRange_GoStringFormat(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()
	bucket := now.Truncate(time.Minute)

	insertMotionRaw(t, db, "cam1", bucket, 0.5)
	insertMotionRaw(t, db, "cam1", bucket.Add(-1*time.Minute), 0.3)

	activity, err := db.GetMotionActivityInRange("cam1", now.Add(-time.Hour), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(activity) != 2 {
		t.Errorf("GetMotionActivityInRange: got %d buckets, want 2", len(activity))
	}
}

func TestCountEventsSince_GoStringFormat(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()

	insertEventRaw(t, db, "ev1", "cam1", "person", now.Add(-1*time.Hour))
	insertEventRaw(t, db, "ev2", "cam1", "car", now.Add(-30*time.Minute))

	count, err := db.CountEventsSince("", now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("CountEventsSince: got %d, want 2", count)
	}
}

func TestQueryEventsInRange_GoStringFormat(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()

	insertEventRaw(t, db, "ev1", "cam1", "person", now.Add(-1*time.Hour))
	insertEventRaw(t, db, "ev2", "cam1", "car", now.Add(-30*time.Minute))

	events, err := db.QueryEventsInRange("cam1", now.Add(-2*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Errorf("QueryEventsInRange: got %d events, want 2", len(events))
	}
}

func TestGetSegmentByID_GoStringFormat(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()

	insertSegmentRaw(t, db, "cam1", "/seg.mp4", now.Add(-10*time.Minute), now, 1000)

	segs, err := db.GetAllSegments("cam1")
	if err != nil || len(segs) == 0 {
		t.Fatal("no segments")
	}

	seg, err := db.GetSegmentByID(segs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if seg == nil {
		t.Fatal("GetSegmentByID returned nil")
	}
	if seg.Camera != "cam1" {
		t.Errorf("Camera = %q, want cam1", seg.Camera)
	}
}
