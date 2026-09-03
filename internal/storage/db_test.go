package storage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/camera"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func makeEvent(id, cam, label string, score float32, ts time.Time) camera.Event {
	return camera.Event{
		ID:         id,
		CameraName: cam,
		Label:      label,
		Score:      score,
		Box:        [4]int{10, 20, 100, 200},
		Timestamp:  ts,
	}
}

func makeSegment(cam, path string, start, end time.Time, size int64) SegmentRecord {
	return SegmentRecord{
		Camera:    cam,
		Path:      path,
		StartTime: start,
		EndTime:   end,
		SizeBytes: size,
	}
}

func mustSaveEvent(t *testing.T, db *DB, e camera.Event) {
	t.Helper()
	if err := db.SaveEvent(e); err != nil {
		t.Fatalf("SaveEvent(%s): %v", e.ID, err)
	}
}

func mustSaveSegment(t *testing.T, db *DB, s SegmentRecord) {
	t.Helper()
	if err := db.SaveSegment(s); err != nil {
		t.Fatalf("SaveSegment(%s): %v", s.Path, err)
	}
}

func TestGetSegmentsEndingBeforeForCamera(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()

	// Seed:
	//   cam1: 2 segments — one 2h old (expired vs cutoff=1h), one 30min old (kept)
	//   cam2: 1 segment 2h old (must NOT appear in cam1 results)
	mustSaveSegment(t, db, makeSegment("cam1", "/cam1/old.mp4", now.Add(-2*time.Hour-10*time.Minute), now.Add(-2*time.Hour), 100))
	mustSaveSegment(t, db, makeSegment("cam1", "/cam1/recent.mp4", now.Add(-30*time.Minute-10*time.Minute), now.Add(-30*time.Minute), 100))
	mustSaveSegment(t, db, makeSegment("cam2", "/cam2/old.mp4", now.Add(-2*time.Hour-10*time.Minute), now.Add(-2*time.Hour), 100))

	cutoff := now.Add(-1 * time.Hour)
	got, err := db.GetSegmentsEndingBeforeForCamera("cam1", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d segments, want 1", len(got))
	}
	if got[0].Path != "/cam1/old.mp4" {
		t.Errorf("got path %q, want /cam1/old.mp4", got[0].Path)
	}
}

// --- Database creation ---

func TestNew_CreatesDatabase(t *testing.T) {
	db := newTestDB(t)
	if db == nil {
		t.Fatal("expected non-nil DB")
	}
}

func TestNew_SchemaHasTables(t *testing.T) {
	db := newTestDB(t)

	// Check events table exists by inserting and querying
	err := db.SaveEvent(makeEvent("schema-test", "cam1", "person", 0.9, time.Now()))
	if err != nil {
		t.Fatalf("events table missing or broken: %v", err)
	}

	// Check segments table exists
	err = db.SaveSegment(makeSegment("cam1", "/tmp/seg.mp4", time.Now(), time.Now().Add(time.Minute), 1024))
	if err != nil {
		t.Fatalf("segments table missing or broken: %v", err)
	}
}

// --- Event operations ---

func TestSaveEvent_GetEventByID_Roundtrip(t *testing.T) {
	db := newTestDB(t)
	ts := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	ev := makeEvent("ev1", "front_door", "person", 0.95, ts)
	ev.SnapshotPath = "/snapshots/ev1.jpg"
	ev.ClipPath = "/clips/ev1.mp4"

	if err := db.SaveEvent(ev); err != nil {
		t.Fatalf("SaveEvent: %v", err)
	}

	got, err := db.GetEventByID("ev1")
	if err != nil {
		t.Fatalf("GetEventByID: %v", err)
	}
	if got == nil {
		t.Fatal("expected event, got nil")
	}
	if got.ID != "ev1" {
		t.Errorf("ID = %q, want %q", got.ID, "ev1")
	}
	if got.CameraName != "front_door" {
		t.Errorf("CameraName = %q, want %q", got.CameraName, "front_door")
	}
	if got.Label != "person" {
		t.Errorf("Label = %q, want %q", got.Label, "person")
	}
	if got.Score != 0.95 {
		t.Errorf("Score = %v, want 0.95", got.Score)
	}
	if got.Box != [4]int{10, 20, 100, 200} {
		t.Errorf("Box = %v, want [10 20 100 200]", got.Box)
	}
	if got.SnapshotPath != "/snapshots/ev1.jpg" {
		t.Errorf("SnapshotPath = %q, want %q", got.SnapshotPath, "/snapshots/ev1.jpg")
	}
	if got.ClipPath != "/clips/ev1.mp4" {
		t.Errorf("ClipPath = %q, want %q", got.ClipPath, "/clips/ev1.mp4")
	}
}

func TestGetEventByID_NotFound(t *testing.T) {
	db := newTestDB(t)
	got, err := db.GetEventByID("nonexistent")
	if err != nil {
		t.Fatalf("GetEventByID: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for nonexistent ID, got %+v", got)
	}
}

func TestQueryEvents_NoFilters(t *testing.T) {
	db := newTestDB(t)
	ts := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)

	for i, label := range []string{"person", "car", "dog"} {
		ev := makeEvent("ev"+string(rune('a'+i)), "cam1", label, 0.9, ts.Add(time.Duration(i)*time.Minute))
		if err := db.SaveEvent(ev); err != nil {
			t.Fatalf("SaveEvent: %v", err)
		}
	}

	events, err := db.QueryEvents("", "", 0, 0)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
}

func TestQueryEvents_FilterByCamera(t *testing.T) {
	db := newTestDB(t)
	ts := time.Now().UTC()

	mustSaveEvent(t, db, makeEvent("e1", "cam1", "person", 0.9, ts))
	mustSaveEvent(t, db, makeEvent("e2", "cam2", "person", 0.8, ts))
	mustSaveEvent(t, db, makeEvent("e3", "cam1", "car", 0.7, ts))

	events, err := db.QueryEvents("cam1", "", 0, 0)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	for _, e := range events {
		if e.CameraName != "cam1" {
			t.Errorf("expected cam1, got %q", e.CameraName)
		}
	}
}

func TestQueryEvents_FilterByLabel(t *testing.T) {
	db := newTestDB(t)
	ts := time.Now().UTC()

	mustSaveEvent(t, db, makeEvent("e1", "cam1", "person", 0.9, ts))
	mustSaveEvent(t, db, makeEvent("e2", "cam1", "car", 0.8, ts))
	mustSaveEvent(t, db, makeEvent("e3", "cam2", "person", 0.7, ts))

	events, err := db.QueryEvents("", "person", 0, 0)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	for _, e := range events {
		if e.Label != "person" {
			t.Errorf("expected label person, got %q", e.Label)
		}
	}
}

func TestQueryEvents_WithLimit(t *testing.T) {
	db := newTestDB(t)
	ts := time.Now().UTC()

	for i := range 5 {
		mustSaveEvent(t, db, makeEvent("e"+string(rune('0'+i)), "cam1", "person", 0.9, ts.Add(time.Duration(i)*time.Minute)))
	}

	events, err := db.QueryEvents("", "", 2, 0)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
}

func TestQueryEvents_FilterByCameraAndLabel(t *testing.T) {
	db := newTestDB(t)
	ts := time.Now().UTC()

	mustSaveEvent(t, db, makeEvent("e1", "cam1", "person", 0.9, ts))
	mustSaveEvent(t, db, makeEvent("e2", "cam1", "car", 0.8, ts))
	mustSaveEvent(t, db, makeEvent("e3", "cam2", "person", 0.7, ts))
	mustSaveEvent(t, db, makeEvent("e4", "cam2", "car", 0.6, ts))

	events, err := db.QueryEvents("cam1", "person", 0, 0)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].ID != "e1" {
		t.Errorf("got ID %q, want e1", events[0].ID)
	}
}

func TestQueryEvents_OrderByTimestampDesc(t *testing.T) {
	db := newTestDB(t)
	t1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	mustSaveEvent(t, db, makeEvent("e1", "cam1", "person", 0.9, t1))
	mustSaveEvent(t, db, makeEvent("e2", "cam1", "person", 0.9, t3))
	mustSaveEvent(t, db, makeEvent("e3", "cam1", "person", 0.9, t2))

	events, err := db.QueryEvents("", "", 0, 0)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if events[0].ID != "e2" || events[1].ID != "e3" || events[2].ID != "e1" {
		t.Errorf("events not ordered by timestamp desc: %v, %v, %v", events[0].ID, events[1].ID, events[2].ID)
	}
}

func TestLatestSnapshotEventSelectsNewestUsableCameraEvidence(t *testing.T) {
	db := newTestDB(t)
	start := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	older := makeEvent("older", "front_door", "person", 0.8, start)
	older.SnapshotAvailable = true
	older.SnapshotPath = "/snapshots/older.jpg"
	newer := makeEvent("newer", "front_door", "car", 0.9, start.Add(time.Minute))
	newer.SnapshotAvailable = true
	newer.SnapshotPath = "/snapshots/newer.jpg"
	unusable := makeEvent("unusable", "front_door", "dog", 0.9, start.Add(2*time.Minute))
	unusable.SnapshotAvailable = true
	otherCamera := makeEvent("other", "garden", "cat", 0.9, start.Add(3*time.Minute))
	otherCamera.SnapshotAvailable = true
	otherCamera.SnapshotPath = "/snapshots/other.jpg"
	for _, event := range []camera.Event{older, newer, unusable, otherCamera} {
		mustSaveEvent(t, db, event)
	}

	got, err := db.LatestSnapshotEvent("front_door")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != "newer" {
		t.Fatalf("latest snapshot = %+v, want newer", got)
	}
	missing, err := db.LatestSnapshotEvent("garage")
	if err != nil || missing != nil {
		t.Fatalf("missing camera snapshot = %+v, err=%v", missing, err)
	}
}

func TestCountEventsToday(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour).Add(1 * time.Hour) // 01:00 today
	yesterday := today.Add(-25 * time.Hour)

	mustSaveEvent(t, db, makeEvent("today1", "cam1", "person", 0.9, today))
	mustSaveEvent(t, db, makeEvent("today2", "cam1", "car", 0.8, today.Add(time.Hour)))
	mustSaveEvent(t, db, makeEvent("yesterday1", "cam1", "person", 0.7, yesterday))

	count, err := db.CountEventsToday("")
	if err != nil {
		t.Fatalf("CountEventsToday: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
}

func TestUpdateEventSnapshotPath(t *testing.T) {
	db := newTestDB(t)
	ev := makeEvent("snap-test", "cam1", "person", 0.9, time.Now().UTC())
	mustSaveEvent(t, db, ev)

	if err := db.UpdateEventSnapshotPath("snap-test", "/snapshots/new.jpg"); err != nil {
		t.Fatalf("UpdateEventSnapshotPath: %v", err)
	}

	got, _ := db.GetEventByID("snap-test")
	if got.SnapshotPath != "/snapshots/new.jpg" {
		t.Errorf("SnapshotPath = %q, want %q", got.SnapshotPath, "/snapshots/new.jpg")
	}
}

// --- Segment operations ---

func TestGetSegmentByPath_Found(t *testing.T) {
	db := newTestDB(t)
	start := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	seg := makeSegment("cam1", "/recordings/cam1/seg001.mp4", start, start.Add(5*time.Minute), 5000)
	mustSaveSegment(t, db, seg)

	got, err := db.GetSegmentByPath("/recordings/cam1/seg001.mp4")
	if err != nil {
		t.Fatalf("GetSegmentByPath: %v", err)
	}
	if got == nil {
		t.Fatal("expected segment, got nil")
	}
	if got.Camera != "cam1" {
		t.Errorf("Camera = %q, want %q", got.Camera, "cam1")
	}
	if got.SizeBytes != 5000 {
		t.Errorf("SizeBytes = %d, want 5000", got.SizeBytes)
	}
}

func TestGetSegmentByPath_NotFound(t *testing.T) {
	db := newTestDB(t)
	got, err := db.GetSegmentByPath("/nonexistent.mp4")
	if err != nil {
		t.Fatalf("GetSegmentByPath: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing path, got %+v", got)
	}
}

func TestGetSegmentsOverlapping(t *testing.T) {
	db := newTestDB(t)
	march20 := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	march21 := march20.AddDate(0, 0, 1)

	mustSaveSegment(t, db, makeSegment("cam1", "/seg1.mp4", march20.Add(14*time.Hour), march20.Add(14*time.Hour+5*time.Minute), 1000))
	mustSaveSegment(t, db, makeSegment("cam1", "/seg2.mp4", march20.Add(15*time.Hour), march20.Add(15*time.Hour+5*time.Minute), 2000))
	mustSaveSegment(t, db, makeSegment("cam1", "/seg3.mp4", march21.Add(10*time.Hour), march21.Add(10*time.Hour+5*time.Minute), 3000))

	segs, err := db.GetSegmentsOverlapping("cam1", march20, march21)
	if err != nil {
		t.Fatalf("GetSegmentsOverlapping: %v", err)
	}
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want 2", len(segs))
	}
}

func TestGetSegmentsOverlapping_SpansRangeBoundary(t *testing.T) {
	db := newTestDB(t)
	dayStart := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.AddDate(0, 0, 1)

	// Starts 5 minutes before the range but runs into it: must be included.
	mustSaveSegment(t, db, makeSegment("cam1", "/cross-in.mp4", dayStart.Add(-5*time.Minute), dayStart.Add(5*time.Minute), 1000))
	// Starts inside the range and crosses out of it: must be included.
	mustSaveSegment(t, db, makeSegment("cam1", "/cross-out.mp4", dayEnd.Add(-5*time.Minute), dayEnd.Add(5*time.Minute), 2000))
	// Ends exactly at range start (half-open): must be excluded.
	mustSaveSegment(t, db, makeSegment("cam1", "/before.mp4", dayStart.Add(-10*time.Minute), dayStart, 3000))
	// Starts exactly at range end (half-open): must be excluded.
	mustSaveSegment(t, db, makeSegment("cam1", "/after.mp4", dayEnd, dayEnd.Add(10*time.Minute), 4000))

	segs, err := db.GetSegmentsOverlapping("cam1", dayStart, dayEnd)
	if err != nil {
		t.Fatalf("GetSegmentsOverlapping: %v", err)
	}
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want 2 (cross-in and cross-out)", len(segs))
	}
	if segs[0].Path != "/cross-in.mp4" || segs[1].Path != "/cross-out.mp4" {
		t.Errorf("got paths %q, %q; want /cross-in.mp4, /cross-out.mp4", segs[0].Path, segs[1].Path)
	}
}

func TestGetSegmentsOverlapping_DifferentCamera(t *testing.T) {
	db := newTestDB(t)
	ts := time.Date(2026, 3, 20, 14, 0, 0, 0, time.UTC)

	mustSaveSegment(t, db, makeSegment("cam1", "/seg1.mp4", ts, ts.Add(5*time.Minute), 1000))
	mustSaveSegment(t, db, makeSegment("cam2", "/seg2.mp4", ts, ts.Add(5*time.Minute), 2000))

	segs, err := db.GetSegmentsOverlapping("cam1", ts.Add(-time.Hour), ts.Add(time.Hour))
	if err != nil {
		t.Fatalf("GetSegmentsOverlapping: %v", err)
	}
	if len(segs) != 1 {
		t.Fatalf("got %d segments, want 1", len(segs))
	}
}

func TestTotalSegmentBytes(t *testing.T) {
	db := newTestDB(t)
	ts := time.Now().UTC()

	mustSaveSegment(t, db, makeSegment("cam1", "/s1.mp4", ts, ts.Add(time.Minute), 1000))
	mustSaveSegment(t, db, makeSegment("cam1", "/s2.mp4", ts, ts.Add(time.Minute), 2000))
	mustSaveSegment(t, db, makeSegment("cam2", "/s3.mp4", ts, ts.Add(time.Minute), 3000))

	total, err := db.TotalSegmentBytes()
	if err != nil {
		t.Fatalf("TotalSegmentBytes: %v", err)
	}
	if total != 6000 {
		t.Errorf("total = %d, want 6000", total)
	}
}

func TestTotalSegmentBytes_EmptyDB(t *testing.T) {
	db := newTestDB(t)
	total, err := db.TotalSegmentBytes()
	if err != nil {
		t.Fatalf("TotalSegmentBytes: %v", err)
	}
	if total != 0 {
		t.Errorf("total = %d, want 0 for empty DB", total)
	}
}

func TestSegmentBytesByCamera(t *testing.T) {
	db := newTestDB(t)
	ts := time.Now().UTC()

	mustSaveSegment(t, db, makeSegment("cam1", "/s1.mp4", ts, ts.Add(time.Minute), 1000))
	mustSaveSegment(t, db, makeSegment("cam1", "/s2.mp4", ts, ts.Add(time.Minute), 2000))
	mustSaveSegment(t, db, makeSegment("cam2", "/s3.mp4", ts, ts.Add(time.Minute), 5000))

	result, err := db.SegmentBytesByCamera()
	if err != nil {
		t.Fatalf("SegmentBytesByCamera: %v", err)
	}
	if result["cam1"] != 3000 {
		t.Errorf("cam1 = %d, want 3000", result["cam1"])
	}
	if result["cam2"] != 5000 {
		t.Errorf("cam2 = %d, want 5000", result["cam2"])
	}
}

func TestCountSegments(t *testing.T) {
	db := newTestDB(t)
	ts := time.Now().UTC()

	mustSaveSegment(t, db, makeSegment("cam1", "/s1.mp4", ts, ts.Add(time.Minute), 100))
	mustSaveSegment(t, db, makeSegment("cam1", "/s2.mp4", ts, ts.Add(time.Minute), 200))
	mustSaveSegment(t, db, makeSegment("cam2", "/s3.mp4", ts, ts.Add(time.Minute), 300))

	count, err := db.CountSegments()
	if err != nil {
		t.Fatalf("CountSegments: %v", err)
	}
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
}

func TestCountSegments_EmptyDB(t *testing.T) {
	db := newTestDB(t)
	count, err := db.CountSegments()
	if err != nil {
		t.Fatalf("CountSegments: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
}

func TestDeleteSegment(t *testing.T) {
	db := newTestDB(t)
	ts := time.Now().UTC()
	mustSaveSegment(t, db, makeSegment("cam1", "/del.mp4", ts, ts.Add(time.Minute), 100))

	if err := db.DeleteSegment("/del.mp4"); err != nil {
		t.Fatalf("DeleteSegment: %v", err)
	}

	got, _ := db.GetSegmentByPath("/del.mp4")
	if got != nil {
		t.Error("segment still exists after delete")
	}
}

func TestGetOldestSegments(t *testing.T) {
	db := newTestDB(t)
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	mustSaveSegment(t, db, makeSegment("cam1", "/s3.mp4", t3, t3.Add(time.Minute), 100))
	mustSaveSegment(t, db, makeSegment("cam1", "/s1.mp4", t1, t1.Add(time.Minute), 100))
	mustSaveSegment(t, db, makeSegment("cam1", "/s2.mp4", t2, t2.Add(time.Minute), 100))

	segs, err := db.GetOldestSegments(2)
	if err != nil {
		t.Fatalf("GetOldestSegments: %v", err)
	}
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want 2", len(segs))
	}
	if segs[0].Path != "/s1.mp4" {
		t.Errorf("oldest segment path = %q, want /s1.mp4", segs[0].Path)
	}
	if segs[1].Path != "/s2.mp4" {
		t.Errorf("second oldest path = %q, want /s2.mp4", segs[1].Path)
	}
}

func TestQuerySegments_TimeRange(t *testing.T) {
	db := newTestDB(t)
	base := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)

	// Segment from 12:00-12:05
	mustSaveSegment(t, db, makeSegment("cam1", "/s1.mp4", base, base.Add(5*time.Minute), 100))
	// Segment from 12:10-12:15
	mustSaveSegment(t, db, makeSegment("cam1", "/s2.mp4", base.Add(10*time.Minute), base.Add(15*time.Minute), 100))
	// Segment from 12:20-12:25
	mustSaveSegment(t, db, makeSegment("cam1", "/s3.mp4", base.Add(20*time.Minute), base.Add(25*time.Minute), 100))

	// Query range 12:03-12:12 should overlap s1 (ends at 12:05 > 12:03) and s2 (starts at 12:10 < 12:12)
	segs, err := db.QuerySegments("cam1", base.Add(3*time.Minute), base.Add(12*time.Minute))
	if err != nil {
		t.Fatalf("QuerySegments: %v", err)
	}
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want 2", len(segs))
	}
}

func TestGetAllSegments(t *testing.T) {
	db := newTestDB(t)
	ts := time.Now().UTC()

	mustSaveSegment(t, db, makeSegment("cam1", "/a.mp4", ts, ts.Add(time.Minute), 100))
	mustSaveSegment(t, db, makeSegment("cam1", "/b.mp4", ts.Add(time.Minute), ts.Add(2*time.Minute), 200))
	mustSaveSegment(t, db, makeSegment("cam2", "/c.mp4", ts, ts.Add(time.Minute), 300))

	segs, err := db.GetAllSegments("cam1")
	if err != nil {
		t.Fatalf("GetAllSegments: %v", err)
	}
	if len(segs) != 2 {
		t.Fatalf("got %d segments for cam1, want 2", len(segs))
	}
}

func TestGetSegmentByID(t *testing.T) {
	db := newTestDB(t)

	now := time.Now().UTC()
	mustSaveSegment(t, db, makeSegment("cam1", "/path/to/seg.mp4", now, now.Add(10*time.Minute), 1000))

	segs, err := db.GetAllSegments("cam1")
	if err != nil || len(segs) == 0 {
		t.Fatal("no segments found")
	}

	got, err := db.GetSegmentByID(segs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected segment, got nil")
	}
	if got.Camera != "cam1" {
		t.Errorf("Camera = %q, want %q", got.Camera, "cam1")
	}

	got, err = db.GetSegmentByID(99999)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Error("expected nil for non-existent ID")
	}
}

// --- GetAdjacentEvents ---

func TestGetAdjacentEvents_MiddleEvent(t *testing.T) {
	db := newTestDB(t)
	t1 := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 3, 20, 11, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)

	mustSaveEvent(t, db, makeEvent("oldest", "cam1", "person", 0.9, t1))
	mustSaveEvent(t, db, makeEvent("middle", "cam1", "person", 0.9, t2))
	mustSaveEvent(t, db, makeEvent("newest", "cam1", "person", 0.9, t3))

	prev, next, err := db.GetAdjacentEvents("middle")
	if err != nil {
		t.Fatalf("GetAdjacentEvents: %v", err)
	}
	if prev != "oldest" {
		t.Errorf("prevID = %q, want %q", prev, "oldest")
	}
	if next != "newest" {
		t.Errorf("nextID = %q, want %q", next, "newest")
	}
}

func TestGetAdjacentEventsFiltered_PreservesReviewContext(t *testing.T) {
	db := newTestDB(t)
	base := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)

	mustSaveEvent(t, db, makeEvent("front-old", "front_door", "person", 0.9, base))
	mustSaveEvent(t, db, makeEvent("driveway-between", "driveway", "person", 0.9, base.Add(time.Minute)))
	mustSaveEvent(t, db, makeEvent("front-middle", "front_door", "person", 0.9, base.Add(2*time.Minute)))
	mustSaveEvent(t, db, makeEvent("front-car", "front_door", "car", 0.9, base.Add(3*time.Minute)))
	mustSaveEvent(t, db, makeEvent("front-new", "front_door", "person", 0.9, base.Add(4*time.Minute)))

	prev, next, err := db.GetAdjacentEventsFiltered("front-middle", EventFilters{
		Camera: "front_door",
		Label:  "person",
	})
	if err != nil {
		t.Fatalf("GetAdjacentEventsFiltered: %v", err)
	}
	if prev != "front-old" {
		t.Errorf("prevID = %q, want front-old", prev)
	}
	if next != "front-new" {
		t.Errorf("nextID = %q, want front-new", next)
	}
}

func TestQueryEventsFiltered_TimeRange(t *testing.T) {
	db := newTestDB(t)
	base := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
	mustSaveEvent(t, db, makeEvent("before", "cam1", "person", 0.9, base.Add(-time.Minute)))
	mustSaveEvent(t, db, makeEvent("inside", "cam1", "person", 0.9, base.Add(time.Minute)))
	mustSaveEvent(t, db, makeEvent("after", "cam1", "person", 0.9, base.Add(3*time.Minute)))

	filters := EventFilters{After: base, Before: base.Add(2 * time.Minute)}
	events, err := db.QueryEventsFiltered(filters, 10, 0)
	if err != nil {
		t.Fatalf("QueryEventsFiltered: %v", err)
	}
	if len(events) != 1 || events[0].ID != "inside" {
		t.Fatalf("events = %+v, want only inside", events)
	}
	if count, err := db.CountEventsFiltered(filters); err != nil || count != 1 {
		t.Fatalf("CountEventsFiltered = %d, %v; want 1, nil", count, err)
	}
}

func TestGetAdjacentEvents_FirstEvent(t *testing.T) {
	db := newTestDB(t)
	t1 := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 3, 20, 11, 0, 0, 0, time.UTC)

	mustSaveEvent(t, db, makeEvent("first", "cam1", "person", 0.9, t1))
	mustSaveEvent(t, db, makeEvent("second", "cam1", "person", 0.9, t2))

	prev, next, err := db.GetAdjacentEvents("first")
	if err != nil {
		t.Fatalf("GetAdjacentEvents: %v", err)
	}
	if prev != "" {
		t.Errorf("prevID = %q, want empty string", prev)
	}
	if next != "second" {
		t.Errorf("nextID = %q, want %q", next, "second")
	}
}

func TestGetAdjacentEvents_LastEvent(t *testing.T) {
	db := newTestDB(t)
	t1 := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 3, 20, 11, 0, 0, 0, time.UTC)

	mustSaveEvent(t, db, makeEvent("first", "cam1", "person", 0.9, t1))
	mustSaveEvent(t, db, makeEvent("last", "cam1", "person", 0.9, t2))

	prev, next, err := db.GetAdjacentEvents("last")
	if err != nil {
		t.Fatalf("GetAdjacentEvents: %v", err)
	}
	if prev != "first" {
		t.Errorf("prevID = %q, want %q", prev, "first")
	}
	if next != "" {
		t.Errorf("nextID = %q, want empty string", next)
	}
}

func TestGetAdjacentEvents_NonexistentID(t *testing.T) {
	db := newTestDB(t)
	mustSaveEvent(t, db, makeEvent("e1", "cam1", "person", 0.9, time.Now().UTC()))

	prev, next, err := db.GetAdjacentEvents("nonexistent")
	if err != nil {
		t.Fatalf("GetAdjacentEvents: %v", err)
	}
	if prev != "" {
		t.Errorf("prevID = %q, want empty string", prev)
	}
	if next != "" {
		t.Errorf("nextID = %q, want empty string", next)
	}
}

// --- GetRecordingDays ---

func TestGetRecordingDays(t *testing.T) {
	db := newTestDB(t)
	march5 := time.Date(2026, 3, 5, 14, 0, 0, 0, time.UTC)
	march10 := time.Date(2026, 3, 10, 10, 0, 0, 0, time.UTC)
	march20 := time.Date(2026, 3, 20, 8, 0, 0, 0, time.UTC)

	mustSaveSegment(t, db, makeSegment("cam1", "/s1.mp4", march5, march5.Add(5*time.Minute), 100))
	mustSaveSegment(t, db, makeSegment("cam1", "/s2.mp4", march10, march10.Add(5*time.Minute), 200))
	mustSaveSegment(t, db, makeSegment("cam1", "/s3.mp4", march20, march20.Add(5*time.Minute), 300))

	days, err := db.GetRecordingDays("cam1", 2026, 3, time.UTC)
	if err != nil {
		t.Fatalf("GetRecordingDays: %v", err)
	}
	if len(days) != 3 {
		t.Fatalf("got %d days, want 3", len(days))
	}
	expected := []int{5, 10, 20}
	for i, d := range days {
		if d != expected[i] {
			t.Errorf("days[%d] = %d, want %d", i, d, expected[i])
		}
	}
}

func TestGetRecordingDays_CameraFilter(t *testing.T) {
	db := newTestDB(t)
	march5 := time.Date(2026, 3, 5, 14, 0, 0, 0, time.UTC)
	march10 := time.Date(2026, 3, 10, 10, 0, 0, 0, time.UTC)

	mustSaveSegment(t, db, makeSegment("cam1", "/s1.mp4", march5, march5.Add(5*time.Minute), 100))
	mustSaveSegment(t, db, makeSegment("cam2", "/s2.mp4", march10, march10.Add(5*time.Minute), 200))

	days, err := db.GetRecordingDays("cam1", 2026, 3, time.UTC)
	if err != nil {
		t.Fatalf("GetRecordingDays: %v", err)
	}
	if len(days) != 1 {
		t.Fatalf("got %d days, want 1", len(days))
	}
	if days[0] != 5 {
		t.Errorf("days[0] = %d, want 5", days[0])
	}
}

func TestGetRecordingDays_EmptyCamera_AllCameras(t *testing.T) {
	db := newTestDB(t)
	march5 := time.Date(2026, 3, 5, 14, 0, 0, 0, time.UTC)
	march10 := time.Date(2026, 3, 10, 10, 0, 0, 0, time.UTC)

	mustSaveSegment(t, db, makeSegment("cam1", "/s1.mp4", march5, march5.Add(5*time.Minute), 100))
	mustSaveSegment(t, db, makeSegment("cam2", "/s2.mp4", march10, march10.Add(5*time.Minute), 200))

	days, err := db.GetRecordingDays("", 2026, 3, time.UTC)
	if err != nil {
		t.Fatalf("GetRecordingDays: %v", err)
	}
	if len(days) != 2 {
		t.Fatalf("got %d days, want 2", len(days))
	}
}

func TestGetRecordingDays_NoData(t *testing.T) {
	db := newTestDB(t)

	days, err := db.GetRecordingDays("cam1", 2026, 6, time.UTC)
	if err != nil {
		t.Fatalf("GetRecordingDays: %v", err)
	}
	if len(days) != 0 {
		t.Errorf("expected empty slice, got %v", days)
	}
}

func TestGetRecordingDays_LocalTimezone(t *testing.T) {
	db := newTestDB(t)
	ams, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}

	// 23:30 UTC March 20 = 00:30 local March 21 (CET, UTC+1): the recording
	// belongs to March 21 on the user's calendar, not March 20.
	lateUTC := time.Date(2026, 3, 20, 23, 30, 0, 0, time.UTC)
	mustSaveSegment(t, db, makeSegment("cam1", "/late.mp4", lateUTC, lateUTC.Add(5*time.Minute), 100))

	days, err := db.GetRecordingDays("cam1", 2026, 3, ams)
	if err != nil {
		t.Fatalf("GetRecordingDays: %v", err)
	}
	if len(days) != 1 || days[0] != 21 {
		t.Errorf("got days %v, want [21] (local calendar day)", days)
	}
}

func TestGetRecordingDays_SegmentSpanningMidnight(t *testing.T) {
	db := newTestDB(t)

	// A segment crossing midnight covers both days.
	start := time.Date(2026, 3, 20, 23, 55, 0, 0, time.UTC)
	mustSaveSegment(t, db, makeSegment("cam1", "/span.mp4", start, start.Add(10*time.Minute), 100))

	days, err := db.GetRecordingDays("cam1", 2026, 3, time.UTC)
	if err != nil {
		t.Fatalf("GetRecordingDays: %v", err)
	}
	if len(days) != 2 || days[0] != 20 || days[1] != 21 {
		t.Errorf("got days %v, want [20 21]", days)
	}
}

// --- QueryEventsInRange ---

func TestQueryEventsInRange(t *testing.T) {
	db := newTestDB(t)
	march20 := time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)
	march21 := march20.AddDate(0, 0, 1)

	mustSaveEvent(t, db, makeEvent("e1", "cam1", "person", 0.9, march20.Add(8*time.Hour)))
	mustSaveEvent(t, db, makeEvent("e2", "cam1", "car", 0.8, march20.Add(20*time.Hour)))
	mustSaveEvent(t, db, makeEvent("e3", "cam1", "person", 0.7, march21.Add(10*time.Hour)))

	events, err := db.QueryEventsInRange("cam1", march20, march21)
	if err != nil {
		t.Fatalf("QueryEventsInRange: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
}

func TestQueryEventsInRange_CameraFilter(t *testing.T) {
	db := newTestDB(t)
	ts := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)

	mustSaveEvent(t, db, makeEvent("e1", "cam1", "person", 0.9, ts))
	mustSaveEvent(t, db, makeEvent("e2", "cam2", "person", 0.8, ts))

	events, err := db.QueryEventsInRange("cam1", ts.Add(-time.Hour), ts.Add(time.Hour))
	if err != nil {
		t.Fatalf("QueryEventsInRange: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].ID != "e1" {
		t.Errorf("got ID %q, want e1", events[0].ID)
	}
}

func TestQueryEventsInRange_NoEvents(t *testing.T) {
	db := newTestDB(t)
	ts := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)

	events, err := db.QueryEventsInRange("cam1", ts.Add(-time.Hour), ts.Add(time.Hour))
	if err != nil {
		t.Fatalf("QueryEventsInRange: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected empty slice, got %v", events)
	}
}

// --- CountEventsByLabel ---

func TestCountEventsByLabel(t *testing.T) {
	db := newTestDB(t)
	ts := time.Now().UTC()

	mustSaveEvent(t, db, makeEvent("e1", "cam1", "person", 0.9, ts))
	mustSaveEvent(t, db, makeEvent("e2", "cam1", "person", 0.8, ts.Add(time.Second)))
	mustSaveEvent(t, db, makeEvent("e3", "cam1", "car", 0.7, ts.Add(2*time.Second)))
	mustSaveEvent(t, db, makeEvent("e4", "cam2", "dog", 0.6, ts.Add(3*time.Second)))

	result, err := db.CountEventsByLabel("")
	if err != nil {
		t.Fatalf("CountEventsByLabel: %v", err)
	}
	if result["person"] != 2 {
		t.Errorf("person count = %d, want 2", result["person"])
	}
	if result["car"] != 1 {
		t.Errorf("car count = %d, want 1", result["car"])
	}
	if result["dog"] != 1 {
		t.Errorf("dog count = %d, want 1", result["dog"])
	}
}

func TestCountEventsByLabel_Empty(t *testing.T) {
	db := newTestDB(t)

	result, err := db.CountEventsByLabel("")
	if err != nil {
		t.Fatalf("CountEventsByLabel: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty map, got %v", result)
	}
}

// --- CountEventsByCamera ---

func TestCountEventsByCamera(t *testing.T) {
	db := newTestDB(t)
	ts := time.Now().UTC()

	mustSaveEvent(t, db, makeEvent("e1", "cam1", "person", 0.9, ts))
	mustSaveEvent(t, db, makeEvent("e2", "cam1", "car", 0.8, ts.Add(time.Second)))
	mustSaveEvent(t, db, makeEvent("e3", "cam2", "person", 0.7, ts.Add(2*time.Second)))

	result, err := db.CountEventsByCamera("")
	if err != nil {
		t.Fatalf("CountEventsByCamera: %v", err)
	}
	if result["cam1"] != 2 {
		t.Errorf("cam1 count = %d, want 2", result["cam1"])
	}
	if result["cam2"] != 1 {
		t.Errorf("cam2 count = %d, want 1", result["cam2"])
	}
}

// --- CountEvents ---

func TestCountEvents(t *testing.T) {
	db := newTestDB(t)
	ts := time.Now().UTC()

	mustSaveEvent(t, db, makeEvent("e1", "cam1", "person", 0.9, ts))
	mustSaveEvent(t, db, makeEvent("e2", "cam1", "car", 0.8, ts.Add(time.Second)))
	mustSaveEvent(t, db, makeEvent("e3", "cam2", "dog", 0.7, ts.Add(2*time.Second)))

	count, err := db.CountEvents("")
	if err != nil {
		t.Fatalf("CountEvents: %v", err)
	}
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
}

func TestCountEvents_EmptyDB(t *testing.T) {
	db := newTestDB(t)

	count, err := db.CountEvents("")
	if err != nil {
		t.Fatalf("CountEvents: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
}

// --- QueryEvents with offset ---

func TestQueryEvents_WithOffset(t *testing.T) {
	db := newTestDB(t)
	ts := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)

	// Insert 5 events with increasing timestamps
	for i := range 5 {
		mustSaveEvent(t, db, makeEvent(
			fmt.Sprintf("ev%d", i),
			"cam1", "person", 0.9,
			ts.Add(time.Duration(i)*time.Minute),
		))
	}

	// QueryEvents orders by timestamp DESC, so ev4 is first, ev0 is last.
	// limit=2, offset=2 should return ev2 and ev1 (3rd and 4th in desc order).
	events, err := db.QueryEvents("", "", 2, 2)
	if err != nil {
		t.Fatalf("QueryEvents with offset: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[0].ID != "ev2" {
		t.Errorf("events[0].ID = %q, want %q", events[0].ID, "ev2")
	}
	if events[1].ID != "ev1" {
		t.Errorf("events[1].ID = %q, want %q", events[1].ID, "ev1")
	}
}

func TestNew_BusyTimeoutIsSet(t *testing.T) {
	db := newTestDB(t)

	var timeout int
	err := db.db.QueryRow("PRAGMA busy_timeout").Scan(&timeout)
	if err != nil {
		t.Fatalf("PRAGMA busy_timeout: %v", err)
	}
	if timeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", timeout)
	}
}

// --- Event EndTime ---

func TestUpdateEventEndTime(t *testing.T) {
	db := newTestDB(t)
	ts := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	ev := makeEvent("end-test", "cam1", "person", 0.9, ts)
	mustSaveEvent(t, db, ev)

	endTime := ts.Add(45 * time.Second)
	if err := db.UpdateEventEndTime("end-test", endTime); err != nil {
		t.Fatalf("UpdateEventEndTime: %v", err)
	}

	got, _ := db.GetEventByID("end-test")
	if got.EndTime.IsZero() {
		t.Fatal("expected EndTime to be set, got zero")
	}
	if !got.EndTime.Equal(endTime) {
		t.Errorf("EndTime = %v, want %v", got.EndTime, endTime)
	}
}

func TestSaveEvent_WithEndTime_Roundtrip(t *testing.T) {
	db := newTestDB(t)
	ts := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	endTime := ts.Add(90 * time.Second)
	ev := makeEvent("endtime-rt", "cam1", "person", 0.9, ts)
	ev.EndTime = endTime
	mustSaveEvent(t, db, ev)

	got, _ := db.GetEventByID("endtime-rt")
	if got.EndTime.IsZero() {
		t.Fatal("expected EndTime to be set")
	}
	if !got.EndTime.Equal(endTime) {
		t.Errorf("EndTime = %v, want %v", got.EndTime, endTime)
	}
}

func TestSaveEvent_WithoutEndTime_ReturnsZero(t *testing.T) {
	db := newTestDB(t)
	ev := makeEvent("no-end", "cam1", "person", 0.9, time.Now().UTC())
	mustSaveEvent(t, db, ev)

	got, _ := db.GetEventByID("no-end")
	if !got.EndTime.IsZero() {
		t.Errorf("expected zero EndTime for event without end, got %v", got.EndTime)
	}
}

func TestQueryEvents_ReturnsEndTime(t *testing.T) {
	db := newTestDB(t)
	ts := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	endTime := ts.Add(30 * time.Second)

	ev := makeEvent("q-end", "cam1", "person", 0.9, ts)
	ev.EndTime = endTime
	mustSaveEvent(t, db, ev)

	events, err := db.QueryEvents("", "", 0, 0)
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if !events[0].EndTime.Equal(endTime) {
		t.Errorf("EndTime = %v, want %v", events[0].EndTime, endTime)
	}
}

func TestQueryEventsInRange_ReturnsEndTime(t *testing.T) {
	db := newTestDB(t)
	ts := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	endTime := ts.Add(60 * time.Second)

	ev := makeEvent("qd-end", "cam1", "person", 0.9, ts)
	ev.EndTime = endTime
	mustSaveEvent(t, db, ev)

	events, err := db.QueryEventsInRange("cam1", ts.Add(-time.Hour), ts.Add(time.Hour))
	if err != nil {
		t.Fatalf("QueryEventsInRange: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if !events[0].EndTime.Equal(endTime) {
		t.Errorf("EndTime = %v, want %v", events[0].EndTime, endTime)
	}
}

func TestMigration_AddsEndTimeColumn(t *testing.T) {
	// Simulate an existing DB without end_time column
	db := newTestDB(t)

	// The migration should have already added end_time.
	// Verify by inserting and reading an event with EndTime.
	ts := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	ev := makeEvent("migrate-test", "cam1", "person", 0.9, ts)
	ev.EndTime = ts.Add(45 * time.Second)
	mustSaveEvent(t, db, ev)

	got, err := db.GetEventByID("migrate-test")
	if err != nil {
		t.Fatalf("GetEventByID: %v", err)
	}
	if got.EndTime.IsZero() {
		t.Fatal("end_time column not working after migration")
	}
}

// --- Auth Users ---

func TestAuthUsers(t *testing.T) {
	db := newTestDB(t)

	// 1. SaveAuthUser creates a user, ListAuthUsers returns it
	if err := db.SaveAuthUser("alice", "hash1"); err != nil {
		t.Fatalf("SaveAuthUser(alice): %v", err)
	}
	users, err := db.ListAuthUsers()
	if err != nil {
		t.Fatalf("ListAuthUsers: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("got %d users, want 1", len(users))
	}
	if users[0].Username != "alice" || users[0].PasswordHash != "hash1" {
		t.Errorf("got user %+v, want alice/hash1", users[0])
	}

	// 2. SaveAuthUser again with same username updates the hash (no duplicate)
	if err := db.SaveAuthUser("alice", "hash2"); err != nil {
		t.Fatalf("SaveAuthUser(alice, hash2): %v", err)
	}
	users, err = db.ListAuthUsers()
	if err != nil {
		t.Fatalf("ListAuthUsers after update: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("got %d users after update, want 1", len(users))
	}
	if users[0].PasswordHash != "hash2" {
		t.Errorf("password_hash = %q, want %q", users[0].PasswordHash, "hash2")
	}

	// 3. SeedAuthUser on existing user does NOT overwrite
	if err := db.SeedAuthUser("alice", "hash3"); err != nil {
		t.Fatalf("SeedAuthUser(alice, hash3): %v", err)
	}
	users, err = db.ListAuthUsers()
	if err != nil {
		t.Fatalf("ListAuthUsers after seed existing: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("got %d users after seed, want 1", len(users))
	}
	if users[0].PasswordHash != "hash2" {
		t.Errorf("SeedAuthUser overwrote existing: password_hash = %q, want %q", users[0].PasswordHash, "hash2")
	}

	// 4. SeedAuthUser on new user inserts it
	if err := db.SeedAuthUser("bob", "bobhash"); err != nil {
		t.Fatalf("SeedAuthUser(bob): %v", err)
	}
	users, err = db.ListAuthUsers()
	if err != nil {
		t.Fatalf("ListAuthUsers after seed new: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("got %d users after seed new, want 2", len(users))
	}

	// 5. After all operations, ListAuthUsers returns correct count and order
	if users[0].Username != "alice" {
		t.Errorf("users[0].Username = %q, want alice", users[0].Username)
	}
	if users[1].Username != "bob" {
		t.Errorf("users[1].Username = %q, want bob", users[1].Username)
	}
}

// --- Motion Activity ---

func TestSaveAndGetMotionActivity(t *testing.T) {
	db := newTestDB(t)
	bucket1 := time.Date(2026, 3, 25, 14, 23, 0, 0, time.UTC)
	bucket2 := time.Date(2026, 3, 25, 14, 24, 0, 0, time.UTC)
	bucket3 := time.Date(2026, 3, 26, 10, 0, 0, 0, time.UTC)
	if err := db.SaveMotionActivity("cam1", bucket1, 0.73); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMotionActivity("cam1", bucket2, 0.12); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMotionActivity("cam1", bucket3, 0.50); err != nil {
		t.Fatal(err)
	}
	march25 := time.Date(2026, 3, 25, 0, 0, 0, 0, time.UTC)
	buckets, err := db.GetMotionActivityInRange("cam1", march25, march25.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 2 {
		t.Fatalf("expected 2 buckets, got %d", len(buckets))
	}
	if buckets[0].Score != 0.73 {
		t.Errorf("expected score 0.73, got %f", buckets[0].Score)
	}
	if buckets[1].Score != 0.12 {
		t.Errorf("expected score 0.12, got %f", buckets[1].Score)
	}
}

func TestSaveMotionActivity_Upsert(t *testing.T) {
	db := newTestDB(t)
	bucket := time.Date(2026, 3, 25, 14, 23, 0, 0, time.UTC)
	if err := db.SaveMotionActivity("cam1", bucket, 0.50); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMotionActivity("cam1", bucket, 0.90); err != nil {
		t.Fatal(err)
	}
	buckets, err := db.GetMotionActivityInRange("cam1", bucket.Add(-time.Hour), bucket.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 1 {
		t.Fatalf("expected 1 bucket, got %d", len(buckets))
	}
	if buckets[0].Score != 0.90 {
		t.Errorf("expected upserted score 0.90, got %f", buckets[0].Score)
	}
}

func TestDeleteMotionActivityBefore(t *testing.T) {
	db := newTestDB(t)
	old := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 3, 25, 12, 0, 0, 0, time.UTC)
	cutoff := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if err := db.SaveMotionActivity("cam1", old, 0.5); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveMotionActivity("cam1", recent, 0.8); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteMotionActivityBefore(cutoff); err != nil {
		t.Fatal(err)
	}
	buckets, err := db.GetMotionActivityInRange("cam1", old.Add(-time.Hour), old.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 0 {
		t.Errorf("expected 0 buckets after cleanup, got %d", len(buckets))
	}
	buckets, err = db.GetMotionActivityInRange("cam1", recent.Add(-time.Hour), recent.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 1 {
		t.Errorf("expected 1 bucket retained, got %d", len(buckets))
	}
}

func TestNew_WALModeIsSet(t *testing.T) {
	db := newTestDB(t)

	var mode string
	err := db.db.QueryRow("PRAGMA journal_mode").Scan(&mode)
	if err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want %q", mode, "wal")
	}
}

// saveTestSegment saves a segment and returns its database ID.
func saveTestSegment(t *testing.T, db *DB, cam, path string, start, end time.Time, size int64) int64 {
	t.Helper()
	mustSaveSegment(t, db, makeSegment(cam, path, start, end, size))
	seg, err := db.GetSegmentByPath(path)
	if err != nil || seg == nil {
		t.Fatalf("saveTestSegment: could not retrieve saved segment %s: %v", path, err)
	}
	return seg.ID
}

// --- Recompression methods ---

func TestGetSegmentsForRecompression(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()

	// Eligible: old enough, not recompressed, 0 failures.
	saveTestSegment(t, db, "cam1", "/tmp/a.mp4", now.Add(-48*time.Hour), now.Add(-47*time.Hour), 1000)

	// Not eligible: already recompressed.
	id2 := saveTestSegment(t, db, "cam1", "/tmp/b.mp4", now.Add(-48*time.Hour), now.Add(-46*time.Hour), 1000)
	_ = db.MarkSegmentRecompressed(id2, 500)

	// Not eligible: 3 failures.
	id3 := saveTestSegment(t, db, "cam1", "/tmp/c.mp4", now.Add(-48*time.Hour), now.Add(-45*time.Hour), 1000)
	for range 3 {
		_ = db.IncrementSegmentRecompressFailures(id3, "")
	}

	// Not eligible: too recent (end_time after cutoff).
	saveTestSegment(t, db, "cam1", "/tmp/d.mp4", now.Add(-2*time.Hour), now.Add(-time.Hour), 1000)

	cutoff := now.Add(-24 * time.Hour)
	segs, err := db.GetSegmentsForRecompression("cam1", cutoff)
	if err != nil {
		t.Fatalf("GetSegmentsForRecompression: %v", err)
	}
	if len(segs) != 1 {
		t.Fatalf("expected 1 eligible segment, got %d", len(segs))
	}
	if segs[0].Path != "/tmp/a.mp4" {
		t.Errorf("expected /tmp/a.mp4, got %s", segs[0].Path)
	}
}

func TestMarkSegmentRecompressed(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	id := saveTestSegment(t, db, "cam1", "/tmp/seg.mp4", now.Add(-48*time.Hour), now.Add(-47*time.Hour), 1000)

	if err := db.MarkSegmentRecompressed(id, 500); err != nil {
		t.Fatalf("MarkSegmentRecompressed: %v", err)
	}

	seg, err := db.GetSegmentByID(id)
	if err != nil {
		t.Fatalf("GetSegmentByID: %v", err)
	}
	if !seg.Recompressed {
		t.Error("expected Recompressed=true")
	}
	if seg.SizeBytes != 500 {
		t.Errorf("SizeBytes = %d, want 500", seg.SizeBytes)
	}
	if seg.RecompressedAt.IsZero() {
		t.Error("expected RecompressedAt to be set")
	}
}

func TestIncrementSegmentRecompressFailures(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	id := saveTestSegment(t, db, "cam1", "/tmp/seg.mp4", now.Add(-48*time.Hour), now.Add(-47*time.Hour), 1000)

	for i := range 3 {
		if err := db.IncrementSegmentRecompressFailures(id, ""); err != nil {
			t.Fatalf("increment %d: %v", i, err)
		}
	}

	// After 3 failures, the segment must not appear in recompression queries.
	cutoff := now.Add(-24 * time.Hour)
	segs, _ := db.GetSegmentsForRecompression("cam1", cutoff)
	for _, s := range segs {
		if s.ID == id {
			t.Error("segment with 3 failures should not be eligible for recompression")
		}
	}
}

func TestSaveSegment_PreservesRecompressionState(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	path := "/tmp/seg.mp4"

	id := saveTestSegment(t, db, "cam1", path, now.Add(-48*time.Hour), now.Add(-47*time.Hour), 1000)
	if err := db.MarkSegmentRecompressed(id, 500); err != nil {
		t.Fatalf("MarkSegmentRecompressed: %v", err)
	}

	// Re-save the segment (simulating a size_bytes update after close).
	mustSaveSegment(t, db, makeSegment("cam1", path, now.Add(-48*time.Hour), now.Add(-47*time.Hour), 999))

	seg, err := db.GetSegmentByID(id)
	if err != nil {
		t.Fatalf("GetSegmentByID: %v", err)
	}
	if !seg.Recompressed {
		t.Error("SaveSegment must not reset Recompressed to false")
	}
	if seg.RecompressedAt.IsZero() {
		t.Error("SaveSegment must not clear RecompressedAt")
	}
	if seg.SizeBytes != 999 {
		t.Errorf("SizeBytes = %d, want 999", seg.SizeBytes)
	}
}

func TestCameraStoppedState(t *testing.T) {
	db := newTestDB(t)

	// Initially no cameras are stopped
	stopped, err := db.ListStoppedCameras()
	if err != nil {
		t.Fatal(err)
	}
	if len(stopped) != 0 {
		t.Fatalf("expected 0 stopped cameras, got %d", len(stopped))
	}

	// Mark a camera as stopped
	if err := db.SetCameraStopped("front_door", true); err != nil {
		t.Fatal(err)
	}

	stopped, err = db.ListStoppedCameras()
	if err != nil {
		t.Fatal(err)
	}
	if len(stopped) != 1 || stopped[0] != "front_door" {
		t.Fatalf("expected [front_door], got %v", stopped)
	}

	// Mark another camera as stopped
	if err := db.SetCameraStopped("backyard", true); err != nil {
		t.Fatal(err)
	}

	stopped, err = db.ListStoppedCameras()
	if err != nil {
		t.Fatal(err)
	}
	if len(stopped) != 2 {
		t.Fatalf("expected 2 stopped cameras, got %d", len(stopped))
	}

	// Resume a camera
	if err := db.SetCameraStopped("front_door", false); err != nil {
		t.Fatal(err)
	}

	stopped, err = db.ListStoppedCameras()
	if err != nil {
		t.Fatal(err)
	}
	if len(stopped) != 1 || stopped[0] != "backyard" {
		t.Fatalf("expected [backyard], got %v", stopped)
	}

	// Idempotent: stopping an already-stopped camera is fine
	if err := db.SetCameraStopped("backyard", true); err != nil {
		t.Fatal(err)
	}
}

// TestEventCategoryRoundTripsAcrossReadPaths guards every event read path
// against column drift: scanEvents-fed queries (QueryEventsFiltered,
// QueryEventsForDate, RecentUnmatchedEventsByLabel) would crash on a missing
// column, and GetEventByID has its own scan that must include category too.
func TestEventCategoryRoundTripsAcrossReadPaths(t *testing.T) {
	db := newTestDB(t)
	ts := time.Now().UTC().Round(0)

	ev := makeEvent("evt-cat-1", "front_door", "car", 0.9, ts)
	ev.Category = "detection"
	ev.SnapshotAvailable = true // RecentUnmatchedEventsByLabel requires this
	if err := db.SaveEvent(ev); err != nil {
		t.Fatalf("SaveEvent: %v", err)
	}

	if got, err := db.GetEventByID("evt-cat-1"); err != nil || got == nil {
		t.Fatalf("GetEventByID: %v (got %v)", err, got)
	} else if got.Category != "detection" {
		t.Errorf("GetEventByID category = %q, want detection", got.Category)
	}

	list, err := db.QueryEventsFiltered(EventFilters{Camera: "front_door"}, 10, 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("QueryEventsFiltered: %v (n=%d)", err, len(list))
	}
	if list[0].Category != "detection" {
		t.Errorf("QueryEventsFiltered category = %q, want detection", list[0].Category)
	}

	if l, _ := db.QueryEventsFiltered(EventFilters{Category: "detection"}, 10, 0); len(l) != 1 {
		t.Errorf("category=detection filter: got %d, want 1", len(l))
	}
	if l, _ := db.QueryEventsFiltered(EventFilters{Category: "alert"}, 10, 0); len(l) != 0 {
		t.Errorf("category=alert filter: got %d, want 0", len(l))
	}

	unmatched, err := db.RecentUnmatchedEventsByLabel("car", 10)
	if err != nil {
		t.Fatalf("RecentUnmatchedEventsByLabel: %v", err)
	}
	if len(unmatched) != 1 || unmatched[0].Category != "detection" {
		t.Fatalf("RecentUnmatchedEventsByLabel: got %d events; category preserved = %v", len(unmatched), len(unmatched) == 1 && unmatched[0].Category == "detection")
	}

	byDate, err := db.QueryEventsInRange("front_door", ts.Add(-time.Hour), ts.Add(time.Hour))
	if err != nil {
		t.Fatalf("QueryEventsInRange: %v", err)
	}
	if len(byDate) != 1 || byDate[0].Category != "detection" {
		t.Errorf("QueryEventsInRange: got %d events, want 1 detection", len(byDate))
	}
}

func TestPushSubscriptionsTable(t *testing.T) {
	db := newTestDB(t)

	_, err := db.Raw().Exec(`INSERT INTO auth_users (username, password_hash) VALUES ('alice', 'hash')`)
	if err != nil {
		t.Fatalf("seed auth_users: %v", err)
	}
	_, err = db.Raw().Exec(`INSERT INTO push_subscriptions (username, endpoint, p256dh, auth, user_agent) VALUES (?, ?, ?, ?, ?)`,
		"alice", "https://fcm.googleapis.com/fcm/send/abc", "pubkey", "authsecret", "iPhone")
	if err != nil {
		t.Fatalf("insert push_subscription: %v", err)
	}
}

func TestNotificationPrefsTable(t *testing.T) {
	db := newTestDB(t)

	_, err := db.Raw().Exec(`INSERT INTO auth_users (username, password_hash) VALUES ('alice', 'hash')`)
	if err != nil {
		t.Fatalf("seed auth_users: %v", err)
	}
	_, err = db.Raw().Exec(`INSERT INTO notification_prefs (username, camera, object_class, enabled) VALUES ('alice', 'front', 'person', 0)`)
	if err != nil {
		t.Fatalf("insert pref: %v", err)
	}
}

func TestPushSubscriptionCRUD(t *testing.T) {
	db := newTestDB(t)

	_, _ = db.Raw().Exec(`INSERT INTO auth_users (username, password_hash) VALUES ('alice', 'hash'), ('bob', 'hash')`)

	// Insert
	id, err := db.SavePushSubscription(PushSubscription{
		Username: "alice", Endpoint: "https://push.example/a", P256dh: "k1", Auth: "a1", UserAgent: "iPhone",
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if id == 0 {
		t.Fatalf("expected nonzero id")
	}

	// Find by endpoint
	got, err := db.FindPushSubscriptionByEndpoint("https://push.example/a")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got == nil || got.Username != "alice" || got.ID != id {
		t.Fatalf("unexpected subscription: %+v", got)
	}

	// Upsert same endpoint, same user → same id, updated keys
	id2, err := db.SavePushSubscription(PushSubscription{
		Username: "alice", Endpoint: "https://push.example/a", P256dh: "k2", Auth: "a2", UserAgent: "iPad",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if id2 != id {
		t.Fatalf("expected id %d, got %d", id, id2)
	}
	got, _ = db.FindPushSubscriptionByEndpoint("https://push.example/a")
	if got.P256dh != "k2" || got.UserAgent != "iPad" {
		t.Fatalf("upsert did not overwrite fields: %+v", got)
	}

	// Upsert same endpoint, different user → ErrSubscriptionOwnedByOther
	_, err = db.SavePushSubscription(PushSubscription{
		Username: "bob", Endpoint: "https://push.example/a", P256dh: "k3", Auth: "a3",
	})
	if err != ErrSubscriptionOwnedByOther {
		t.Fatalf("expected ErrSubscriptionOwnedByOther, got %v", err)
	}

	// List by user
	_, _ = db.SavePushSubscription(PushSubscription{
		Username: "alice", Endpoint: "https://push.example/b", P256dh: "k", Auth: "a",
	})
	list, err := db.ListPushSubscriptionsByUser("alice")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 subs, got %d", len(list))
	}

	// Delete by id + user (wrong user)
	err = db.DeletePushSubscription(id, "bob")
	if err != ErrPushSubscriptionNotFound {
		t.Fatalf("expected ErrPushSubscriptionNotFound for wrong-user delete, got %v", err)
	}

	// Delete by id + user (right user)
	if err := db.DeletePushSubscription(id, "alice"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, _ = db.FindPushSubscriptionByEndpoint("https://push.example/a")
	if got != nil {
		t.Fatalf("expected nil after delete, got %+v", got)
	}

	// Delete by endpoint (used by dispatcher on 410)
	if err := db.DeletePushSubscriptionByEndpoint("https://push.example/b"); err != nil {
		t.Fatalf("delete by endpoint: %v", err)
	}
}

func TestNotificationPrefsCRUD(t *testing.T) {
	db := newTestDB(t)
	_, _ = db.Raw().Exec(`INSERT INTO auth_users (username, password_hash) VALUES ('alice', 'hash')`)

	// Default: no rows → IsNotificationEnabled returns true for any (camera, class)
	enabled, err := db.IsNotificationEnabled("alice", "front", "person")
	if err != nil {
		t.Fatalf("enabled: %v", err)
	}
	if !enabled {
		t.Fatalf("default should be enabled")
	}

	// Opt out specific (camera, class)
	if err := db.SetNotificationPref("alice", "front", "person", false); err != nil {
		t.Fatalf("set: %v", err)
	}
	enabled, _ = db.IsNotificationEnabled("alice", "front", "person")
	if enabled {
		t.Fatalf("expected disabled")
	}
	// Sibling class still enabled
	enabled, _ = db.IsNotificationEnabled("alice", "front", "car")
	if !enabled {
		t.Fatalf("sibling class should still be enabled")
	}

	// Wildcard disable beats specific enable
	_ = db.SetNotificationPref("alice", "back", "*", false)
	_ = db.SetNotificationPref("alice", "back", "person", true)
	enabled, _ = db.IsNotificationEnabled("alice", "back", "person")
	if enabled {
		t.Fatalf("wildcard disable should beat specific enable")
	}

	// List all prefs.
	// Only two disabled rows are stored: (front,person,0) and (back,*,0).
	// The (back,person,true) call above is a no-op because the sparse-table
	// contract stores only disabled rows — a request to enable something
	// that's already default-enabled DELETEs any existing row (there is none).
	list, err := db.ListNotificationPrefs("alice")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(list))
	}

	// Re-setting to default (enabled=true) should remove the row to keep the table sparse
	_ = db.SetNotificationPref("alice", "front", "person", true)
	list, _ = db.ListNotificationPrefs("alice")
	for _, p := range list {
		if p.Camera == "front" && p.ObjectClass == "person" {
			t.Fatalf("expected row to be removed when reset to default, got %+v", p)
		}
	}
}

// --- Emergency cleanup and size-priority recompression queries ---

func TestGetOldestSegmentsOlderThan(t *testing.T) {
	db := newTestDB(t)
	base := time.Now().UTC()

	// Three segments at -4h, -2h, -30m relative to now.
	mustSaveSegment(t, db, makeSegment("a", "/old.mp4", base.Add(-4*time.Hour-10*time.Minute), base.Add(-4*time.Hour), 100))
	mustSaveSegment(t, db, makeSegment("a", "/mid.mp4", base.Add(-2*time.Hour-10*time.Minute), base.Add(-2*time.Hour), 100))
	mustSaveSegment(t, db, makeSegment("a", "/new.mp4", base.Add(-40*time.Minute), base.Add(-30*time.Minute), 100))

	// cutoff = now-1h: segments ending before it are old enough for deletion;
	// "old" (-4h) and "mid" (-2h) qualify, "new" (-30m) is protected.
	segs, err := db.GetOldestSegmentsOlderThan(10, base.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want 2", len(segs))
	}
	if segs[0].Path != "/old.mp4" {
		t.Fatalf("first = %q, want /old.mp4", segs[0].Path)
	}
	if segs[1].Path != "/mid.mp4" {
		t.Fatalf("second = %q, want /mid.mp4", segs[1].Path)
	}
}

func TestGetLargestSegmentSizeSince(t *testing.T) {
	db := newTestDB(t)
	base := time.Now().UTC()

	mustSaveSegment(t, db, makeSegment("a", "/s.mp4", base.Add(-2*time.Hour), base.Add(-2*time.Hour+time.Minute), 100))
	mustSaveSegment(t, db, makeSegment("a", "/m.mp4", base.Add(-30*time.Minute), base.Add(-30*time.Minute+time.Minute), 500))
	mustSaveSegment(t, db, makeSegment("a", "/l.mp4", base.Add(-5*time.Minute), base.Add(-5*time.Minute+time.Minute), 2000))

	// Only segments with start_time after now-1h are in scope: /m.mp4 (500) and /l.mp4 (2000).
	got, err := db.GetLargestSegmentSizeSince(base.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got != 2000 {
		t.Fatalf("GetLargestSegmentSizeSince = %d, want 2000", got)
	}
}

func TestGetLargestSegmentSizeSince_NoRows(t *testing.T) {
	db := newTestDB(t)

	// Empty DB — must return 0, not an error.
	got, err := db.GetLargestSegmentSizeSince(time.Now().UTC())
	if err != nil {
		t.Fatalf("GetLargestSegmentSizeSince on empty DB: %v", err)
	}
	if got != 0 {
		t.Fatalf("GetLargestSegmentSizeSince on empty DB = %d, want 0", got)
	}
}

func TestGetRecompressionCandidatesBySize(t *testing.T) {
	db := newTestDB(t)
	base := time.Now().UTC().Add(-4 * 24 * time.Hour)

	mustSaveSegment(t, db, makeSegment("a", "/small.mp4", base, base.Add(time.Minute), 10))
	mustSaveSegment(t, db, makeSegment("a", "/big.mp4", base, base.Add(time.Minute), 500))
	mustSaveSegment(t, db, makeSegment("a", "/mid.mp4", base, base.Add(time.Minute), 100))

	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	segs, err := db.GetRecompressionCandidatesBySize("a", cutoff, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 3 {
		t.Fatalf("got %d, want 3", len(segs))
	}
	if segs[0].Path != "/big.mp4" || segs[1].Path != "/mid.mp4" || segs[2].Path != "/small.mp4" {
		t.Fatalf("order = %v, want big,mid,small", []string{segs[0].Path, segs[1].Path, segs[2].Path})
	}
}

func mustInsertEvent(t *testing.T, db *DB, cam string, ts, end time.Time, clip, snap string) {
	t.Helper()
	// Include clip and snap in the ID to keep events unique when timestamp is the same.
	ev := camera.Event{
		ID:           fmt.Sprintf("%s-%s-%s-%s", cam, ts.Format("20060102T150405.000"), clip, snap),
		CameraName:   cam,
		Label:        "person",
		Score:        0.9,
		Timestamp:    ts,
		ClipPath:     clip,
		SnapshotPath: snap,
	}
	if !end.IsZero() {
		ev.EndTime = end
	}
	if err := db.SaveEvent(ev); err != nil {
		t.Fatal(err)
	}
}

func TestClipsByCameraInRange(t *testing.T) {
	db := newTestDB(t)
	mid := time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)

	// Event with clip_path, end_time inside window.
	mustInsertEvent(t, db, "cam-a", mid, mid.Add(30*time.Second), "/c/1.mp4", "/s/1.jpg")
	// Event with NULL end_time but timestamp outside window.
	mustInsertEvent(t, db, "cam-a", mid.AddDate(0, 0, -5), time.Time{}, "/c/2.mp4", "")
	// Snapshot-only event inside window.
	mustInsertEvent(t, db, "cam-a", mid, time.Time{}, "", "/s/3.jpg")

	got, err := db.ClipsByCameraInRange("cam-a",
		mid.AddDate(0, 0, -1),
		mid.AddDate(0, 0, +1))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (one with clip, one snapshot-only)", len(got))
	}
}

func TestClipsByCamera_NoWindow(t *testing.T) {
	db := newTestDB(t)
	mustInsertEvent(t, db, "cam-a", time.Now(), time.Time{}, "/c/1.mp4", "")
	mustInsertEvent(t, db, "cam-a", time.Now().Add(time.Second), time.Time{}, "", "") // no media
	mustInsertEvent(t, db, "cam-b", time.Now().Add(2*time.Second), time.Time{}, "/c/2.mp4", "")

	got, err := db.ClipsByCamera("cam-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d, want 1 (only events with media for cam-a)", len(got))
	}
}

func TestClipsByCameraOlderThan(t *testing.T) {
	db := newTestDB(t)
	mid := time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)

	// cam-a: before cutoff — no end_time, COALESCE falls back to timestamp.
	mustInsertEvent(t, db, "cam-a", mid.AddDate(0, 0, -5), time.Time{}, "/c/before.mp4", "")
	// cam-a: after cutoff — should be excluded.
	mustInsertEvent(t, db, "cam-a", mid.AddDate(0, 0, +5), time.Time{}, "/c/after.mp4", "")
	// cam-b: before cutoff — cross-camera isolation check.
	mustInsertEvent(t, db, "cam-b", mid.AddDate(0, 0, -5), time.Time{}, "/c/other.mp4", "")
	// cam-a: before cutoff but no media — media-predicate check.
	mustInsertEvent(t, db, "cam-a", mid.AddDate(0, 0, -3), time.Time{}, "", "")

	got, err := db.ClipsByCameraOlderThan("cam-a", mid)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	wantID := fmt.Sprintf("cam-a-%s-%s-%s", mid.AddDate(0, 0, -5).Format("20060102T150405.000"), "/c/before.mp4", "")
	if got[0].ID != wantID {
		t.Errorf("got ID %q, want %q", got[0].ID, wantID)
	}
}

func mustInsertSegment(t *testing.T, db *DB, camera string, start time.Time, size int64) {
	t.Helper()
	if err := db.SaveSegment(SegmentRecord{
		Camera:    camera,
		Path:      "/tmp/" + camera + "-" + start.Format("20060102T150405") + ".mp4",
		StartTime: start,
		EndTime:   start.Add(time.Minute),
		SizeBytes: size,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSegmentsByCameraOlderThan(t *testing.T) {
	db := newTestDB(t)
	mustInsertSegment(t, db, "cam-a", time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), 1<<20)
	mustInsertSegment(t, db, "cam-a", time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), 2<<20)
	mustInsertSegment(t, db, "cam-b", time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), 4<<20)

	got, err := db.SegmentsByCameraOlderThan("cam-a", time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d segments, want 1", len(got))
	}
	if got[0].Camera != "cam-a" || got[0].SizeBytes != 1<<20 {
		t.Errorf("unexpected segment: %+v", got[0])
	}
}

func TestSegmentsByCameraInRange(t *testing.T) {
	db := newTestDB(t)
	mid := time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)
	mustInsertSegment(t, db, "cam-a", mid.AddDate(0, 0, -5), 1)
	mustInsertSegment(t, db, "cam-a", mid, 2)
	mustInsertSegment(t, db, "cam-a", mid.AddDate(0, 0, +5), 4)

	got, err := db.SegmentsByCameraInRange("cam-a",
		mid.AddDate(0, 0, -1),
		mid.AddDate(0, 0, +1))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SizeBytes != 2 {
		t.Errorf("got %v, want exactly the mid segment", got)
	}
}

func TestOldestSegmentsUntilBytes(t *testing.T) {
	db := newTestDB(t)
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	mustInsertSegment(t, db, "cam-a", base.AddDate(0, 0, 0), 100)
	mustInsertSegment(t, db, "cam-a", base.AddDate(0, 0, 1), 200)
	mustInsertSegment(t, db, "cam-b", base.AddDate(0, 0, 2), 400)

	got, err := db.OldestSegmentsUntilBytes(250)
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, s := range got {
		sum += s.SizeBytes
	}
	if sum < 250 {
		t.Errorf("sum %d under target", sum)
	}
	for i := 1; i < len(got); i++ {
		if got[i].StartTime.Before(got[i-1].StartTime) {
			t.Errorf("not oldest-first at index %d", i)
		}
	}
}

func TestStorageAudit_InsertAndList(t *testing.T) {
	db := newTestDB(t)
	ts := time.Now().UTC()
	if err := db.InsertStorageAudit(StorageAuditEntry{
		Timestamp: ts,
		Actor:     "admin",
		ScopeJSON: `{"camera":"cam-a","older_than_days":3}`,
		Bytes:     1 << 30,
		Files:     42,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := db.StorageAudit(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	e := got[0]
	if e.Actor != "admin" {
		t.Errorf("Actor = %q, want %q", e.Actor, "admin")
	}
	if e.Bytes != 1<<30 {
		t.Errorf("Bytes = %d, want %d", e.Bytes, int64(1)<<30)
	}
	if e.Files != 42 {
		t.Errorf("Files = %d, want 42", e.Files)
	}
	if e.ScopeJSON != `{"camera":"cam-a","older_than_days":3}` {
		t.Errorf("ScopeJSON mismatch: %q", e.ScopeJSON)
	}
	if !e.Timestamp.UTC().Equal(ts.Truncate(time.Second)) && !e.Timestamp.UTC().Equal(ts) {
		t.Errorf("Timestamp = %v, want ~%v", e.Timestamp, ts)
	}
}

func TestPerDayCameraSegmentBytes(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()

	// Every timestamp is anchored to midday so the segments a day is
	// supposed to hold stay on that day whatever hour the suite runs at.
	// Taking the time of day from the clock instead puts the "one hour
	// later" segment on the next date for the hour before midnight UTC,
	// which is a suite that fails for one hour out of every twenty-four.
	// The newest day is yesterday, so no segment is dated in the future.
	newest := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC).AddDate(0, 0, -1)

	// Three distinct UTC days inside the 30-day window, plus one segment
	// well outside it. Segments are written through the production path
	// (SaveSegment -> utc()), so start_time is stored exactly as it is in
	// production with the modernc.org/sqlite driver.
	twoDaysBefore := newest.AddDate(0, 0, -2)
	oneDayBefore := newest.AddDate(0, 0, -1)
	mustSaveSegment(t, db, makeSegment("cam1", "/cam1/d2.mp4", twoDaysBefore, twoDaysBefore.Add(time.Minute), 1000))
	mustSaveSegment(t, db, makeSegment("cam1", "/cam1/d1a.mp4", oneDayBefore, oneDayBefore.Add(time.Minute), 3000))
	mustSaveSegment(t, db, makeSegment("cam1", "/cam1/d1b.mp4", oneDayBefore.Add(time.Hour), oneDayBefore.Add(time.Hour+time.Minute), 2000))
	mustSaveSegment(t, db, makeSegment("cam1", "/cam1/d0.mp4", newest, newest.Add(time.Minute), 5000))
	// 40 days before the newest day: outside the 30-day window, must be excluded.
	old := newest.AddDate(0, 0, -40)
	mustSaveSegment(t, db, makeSegment("cam1", "/cam1/old.mp4", old, old.Add(time.Minute), 9999))
	// Different camera, same recent day: must not leak into cam1 totals.
	mustSaveSegment(t, db, makeSegment("cam2", "/cam2/x.mp4", newest, newest.Add(time.Minute), 7777))

	// The three days the query must report have to be genuinely distinct,
	// or the row-count assertion below would pass on a single merged day.
	for _, pair := range [][2]time.Time{{twoDaysBefore, oneDayBefore}, {oneDayBefore, newest}} {
		if pair[0].Format("2006-01-02") == pair[1].Format("2006-01-02") {
			t.Fatalf("test dates collapsed onto one day: %s", pair[0].Format("2006-01-02"))
		}
	}
	// The excluded segment has to fall outside the window the query uses,
	// which is measured from the moment of the call, not from newest.
	if !old.Before(now.AddDate(0, 0, -30)) {
		t.Fatalf("the %q segment is inside the 30-day window, so excluding it proves nothing", old.Format("2006-01-02"))
	}

	rows, err := db.PerDayCameraSegmentBytes("cam1", 30)
	if err != nil {
		t.Fatalf("PerDayCameraSegmentBytes: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d day rows, want 3: %+v", len(rows), rows)
	}

	var total int64
	for i, r := range rows {
		if r.Date == "" {
			t.Errorf("row %d has empty Date: %+v", i, r)
		}
		if i > 0 && rows[i-1].Date >= r.Date {
			t.Errorf("rows not ascending by date: %q then %q", rows[i-1].Date, r.Date)
		}
		total += r.Bytes
	}
	if total != 11000 {
		t.Errorf("summed bytes = %d, want 11000 (old segment and cam2 excluded)", total)
	}

	want := map[string]int64{
		twoDaysBefore.Format("2006-01-02"): 1000,
		oneDayBefore.Format("2006-01-02"):  5000,
		newest.Format("2006-01-02"):        5000,
	}
	for _, r := range rows {
		if w, ok := want[r.Date]; !ok {
			t.Errorf("unexpected day %q in result", r.Date)
		} else if r.Bytes != w {
			t.Errorf("day %q bytes = %d, want %d", r.Date, r.Bytes, w)
		}
	}
}

// QueryRowForTest exposes the wrapped *sql.DB for assertions on columns not
// surfaced by a typed query.
func (d *DB) QueryRowForTest(query string, args ...any) *sql.Row {
	return d.db.QueryRow(query, args...)
}

// --- SetEventClip / ClearEventClip ---

func TestSetEventClip_PersistsAndResetsState(t *testing.T) {
	db := newTestDB(t)
	ev := makeEvent("clip-set", "cam1", "person", 0.9, time.Now().UTC())
	mustSaveEvent(t, db, ev)

	// Pretend a prior recompression happened.
	if err := db.MarkClipRecompressed("clip-set", 111); err != nil {
		t.Fatalf("MarkClipRecompressed: %v", err)
	}

	if err := db.SetEventClip("clip-set", "/clips/new.mp4", 4242); err != nil {
		t.Fatalf("SetEventClip: %v", err)
	}

	got, _ := db.GetEventByID("clip-set")
	if got.ClipPath != "/clips/new.mp4" || !got.ClipAvailable {
		t.Errorf("ClipPath=%q available=%v, want /clips/new.mp4 true", got.ClipPath, got.ClipAvailable)
	}
	st, ok, err := db.GetClipRecompressState("clip-set")
	if err != nil || !ok {
		t.Fatalf("GetClipRecompressState: ok=%v err=%v", ok, err)
	}
	if st.Recompressed {
		t.Error("SetEventClip must reset recompressed to false")
	}
	var size int64
	if err := db.QueryRowForTest("SELECT clip_size_bytes FROM events WHERE id = ?", "clip-set").Scan(&size); err != nil {
		t.Fatal(err)
	}
	if size != 4242 {
		t.Errorf("clip_size_bytes = %d, want 4242", size)
	}
	var at sql.NullTime
	var failures int
	if err := db.QueryRowForTest(
		"SELECT recompressed_at, recompress_failures FROM events WHERE id = ?", "clip-set",
	).Scan(&at, &failures); err != nil {
		t.Fatal(err)
	}
	if at.Valid {
		t.Errorf("recompressed_at = %v, want NULL", at.Time)
	}
	if failures != 0 {
		t.Errorf("recompress_failures = %d, want 0", failures)
	}
}

// mustClipEvent saves an event with an end time and an attached clip of the
// given size, making it a recompression candidate.
func mustClipEvent(t *testing.T, db *DB, id, cam string, endTime time.Time, size int64) {
	t.Helper()
	ev := makeEvent(id, cam, "person", 0.9, endTime.Add(-time.Minute))
	ev.EndTime = endTime
	mustSaveEvent(t, db, ev)
	if err := db.SetEventClip(id, "/clips/"+id+".mp4", size); err != nil {
		t.Fatalf("SetEventClip(%s): %v", id, err)
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestClearEventClip_ZeroesPathSizeAndState(t *testing.T) {
	db := newTestDB(t)
	ev := makeEvent("clip-clear", "cam1", "person", 0.9, time.Now().UTC())
	mustSaveEvent(t, db, ev)
	if err := db.SetEventClip("clip-clear", "/clips/x.mp4", 999); err != nil {
		t.Fatalf("SetEventClip: %v", err)
	}

	// Drive failure counter to a non-zero value to verify ClearEventClip resets it.
	for i := 0; i < 2; i++ {
		if err := db.IncrementClipRecompressFailures("clip-clear", ""); err != nil {
			t.Fatalf("IncrementClipRecompressFailures: %v", err)
		}
	}

	if err := db.ClearEventClip("clip-clear"); err != nil {
		t.Fatalf("ClearEventClip: %v", err)
	}

	got, _ := db.GetEventByID("clip-clear")
	if got.ClipPath != "" || got.ClipAvailable {
		t.Errorf("after clear ClipPath=%q available=%v, want empty/false", got.ClipPath, got.ClipAvailable)
	}
	var size int64
	var failures int
	if err := db.QueryRowForTest(
		"SELECT clip_size_bytes, recompress_failures FROM events WHERE id = ?", "clip-clear",
	).Scan(&size, &failures); err != nil {
		t.Fatal(err)
	}
	if size != 0 {
		t.Errorf("clip_size_bytes = %d, want 0", size)
	}
	if failures != 0 {
		t.Errorf("recompress_failures = %d, want 0", failures)
	}
}

// --- clip recompression candidate queries ---

func TestClipCandidates_EligibilityAndOrdering(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)

	mustClipEvent(t, db, "big", "cam1", old, 500)                  // eligible, large
	mustClipEvent(t, db, "small", "cam1", old.Add(time.Hour), 100) // eligible, smaller, newer
	mustClipEvent(t, db, "toonew", "cam1", now, 900)               // too new (after cutoff)

	// Ineligible: already recompressed.
	mustClipEvent(t, db, "done", "cam1", old, 700)
	if err := db.MarkClipRecompressed("done", 70); err != nil {
		t.Fatal(err)
	}
	// Ineligible: at the failure cap.
	mustClipEvent(t, db, "stuck", "cam1", old, 800)
	for i := 0; i < 3; i++ {
		if err := db.IncrementClipRecompressFailures("stuck", ""); err != nil {
			t.Fatal(err)
		}
	}
	// Ineligible: clip not available.
	mustClipEvent(t, db, "gone", "cam1", old, 600)
	if err := db.ClearEventClip("gone"); err != nil {
		t.Fatal(err)
	}

	cutoff := now.Add(-1 * time.Hour)

	bySize, err := db.GetClipRecompressionCandidatesBySize("cam1", cutoff, 10)
	if err != nil {
		t.Fatalf("GetClipRecompressionCandidatesBySize: %v", err)
	}
	gotIDs := func(cs []ClipRecord) []string {
		out := make([]string, len(cs))
		for i, c := range cs {
			out[i] = c.EventID
		}
		return out
	}
	if want := []string{"big", "small"}; !equalStrs(gotIDs(bySize), want) {
		t.Errorf("by size = %v, want %v", gotIDs(bySize), want)
	}

	byAge, err := db.GetClipsForRecompression("cam1", cutoff)
	if err != nil {
		t.Fatalf("GetClipsForRecompression: %v", err)
	}
	if want := []string{"big", "small"}; !equalStrs(gotIDs(byAge), want) {
		t.Errorf("by age = %v, want %v", gotIDs(byAge), want)
	}
}

// --- clip recompress mark / increment / reset ---

func TestMarkClipRecompressed_SetsFlagTimestampSize(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()
	mustClipEvent(t, db, "mc", "cam1", now.Add(-48*time.Hour), 1000)

	if err := db.MarkClipRecompressed("mc", 250); err != nil {
		t.Fatalf("MarkClipRecompressed: %v", err)
	}

	st, ok, err := db.GetClipRecompressState("mc")
	if err != nil || !ok {
		t.Fatalf("GetClipRecompressState: ok=%v err=%v", ok, err)
	}
	if !st.Recompressed {
		t.Error("expected recompressed=true")
	}
	var size int64
	var at sql.NullTime
	if err := db.QueryRowForTest("SELECT clip_size_bytes, recompressed_at FROM events WHERE id = ?", "mc").Scan(&size, &at); err != nil {
		t.Fatal(err)
	}
	if size != 250 {
		t.Errorf("clip_size_bytes = %d, want 250", size)
	}
	if !at.Valid {
		t.Error("expected recompressed_at to be set")
	}
}

func TestResetStuckClipRecompressFailures_ClearsCappedClips(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()
	mustClipEvent(t, db, "stuck", "cam1", now.Add(-48*time.Hour), 1000)
	for i := 0; i < 3; i++ {
		if err := db.IncrementClipRecompressFailures("stuck", ""); err != nil {
			t.Fatal(err)
		}
	}
	// Not eligible while at the cap.
	if got, _ := db.GetClipsForRecompression("cam1", now.Add(-time.Hour)); len(got) != 0 {
		t.Fatalf("expected 0 eligible at cap, got %d", len(got))
	}

	reset, err := db.ResetStuckClipRecompressFailures(nil)
	if err != nil {
		t.Fatalf("ResetStuckClipRecompressFailures: %v", err)
	}
	if reset != 1 {
		t.Errorf("reset = %d, want 1", reset)
	}
	if got, _ := db.GetClipsForRecompression("cam1", now.Add(-time.Hour)); len(got) != 1 {
		t.Errorf("expected 1 eligible after reset, got %d", len(got))
	}
}

// --- GetClipRecompressState ---

func TestGetClipRecompressState(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()
	mustClipEvent(t, db, "rs", "cam1", now.Add(-48*time.Hour), 1000)

	st, ok, err := db.GetClipRecompressState("rs")
	if err != nil || !ok {
		t.Fatalf("GetClipRecompressState: ok=%v err=%v", ok, err)
	}
	if st.ClipPath != "/clips/rs.mp4" || !st.ClipAvailable || st.Recompressed {
		t.Errorf("state = %+v, want path /clips/rs.mp4, available true, recompressed false", st)
	}

	_, ok, err = db.GetClipRecompressState("does-not-exist")
	if err != nil {
		t.Fatalf("err for missing row: %v", err)
	}
	if ok {
		t.Error("expected ok=false for missing row")
	}
}

// --- BackfillClipSizes ---

func TestBackfillClipSizes(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	now := time.Now().UTC()

	// Real file on disk, but the row has clip_size_bytes = 0 (legacy).
	realPath := filepath.Join(dir, "real.mp4")
	if err := os.WriteFile(realPath, make([]byte, 321), 0o644); err != nil {
		t.Fatal(err)
	}
	evReal := makeEvent("real", "cam1", "person", 0.9, now.Add(-time.Hour))
	evReal.EndTime = now.Add(-time.Hour)
	mustSaveEvent(t, db, evReal)
	if _, err := db.db.Exec("UPDATE events SET clip_path = ?, clip_available = 1, clip_size_bytes = 0 WHERE id = ?", realPath, "real"); err != nil {
		t.Fatal(err)
	}

	// Available row whose file is missing: must be reconciled to unavailable.
	evGhost := makeEvent("ghost", "cam1", "person", 0.9, now.Add(-time.Hour))
	evGhost.EndTime = now.Add(-time.Hour)
	mustSaveEvent(t, db, evGhost)
	if _, err := db.db.Exec("UPDATE events SET clip_path = ?, clip_available = 1, clip_size_bytes = 0 WHERE id = ?", filepath.Join(dir, "missing.mp4"), "ghost"); err != nil {
		t.Fatal(err)
	}

	examined, err := db.BackfillClipSizes(100)
	if err != nil {
		t.Fatalf("BackfillClipSizes: %v", err)
	}
	if examined != 2 {
		t.Errorf("examined = %d, want 2", examined)
	}

	var realSize int64
	if err := db.QueryRowForTest("SELECT clip_size_bytes FROM events WHERE id = ?", "real").Scan(&realSize); err != nil {
		t.Fatal(err)
	}
	if realSize != 321 {
		t.Errorf("real clip_size_bytes = %d, want 321", realSize)
	}

	st, _, _ := db.GetClipRecompressState("ghost")
	if st.ClipAvailable {
		t.Error("ghost clip should be reconciled to unavailable")
	}

	// A second pass examines nothing (real now has a size, ghost is unavailable).
	examined, err = db.BackfillClipSizes(100)
	if err != nil {
		t.Fatal(err)
	}
	if examined != 0 {
		t.Errorf("second pass examined = %d, want 0", examined)
	}
}

// Timestamps are stored in the driver's canonical UTC text form, so a
// timestamp predicate compares text. Binding a time.Time that carries a zone
// offset serializes it with that offset and compares it against UTC text, which
// is a different string: the filter then silently returns the wrong rows. Every
// timestamp bind has to be normalized with utc() first.
func TestQueryEventsFiltered_TimeWindowIgnoresCallerTimezone(t *testing.T) {
	db := newTestDB(t)

	base := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	for i, name := range []string{"before", "inside", "after"} {
		ev := makeEvent(name, "cam", "person", 0.9, base.Add(time.Duration(i)*time.Hour))
		if err := db.SaveEvent(ev); err != nil {
			t.Fatalf("SaveEvent(%s): %v", name, err)
		}
	}

	// The same instants, expressed in a zone a browser or an API caller would
	// plausibly send. Only the text representation differs.
	east := time.FixedZone("UTC+9", 9*3600)
	window := EventFilters{
		After:  base.Add(30 * time.Minute).In(east),
		Before: base.Add(90 * time.Minute).In(east),
	}

	events, err := db.QueryEventsFiltered(window, 100, 0)
	if err != nil {
		t.Fatalf("QueryEventsFiltered: %v", err)
	}
	if len(events) != 1 || events[0].ID != "inside" {
		got := make([]string, len(events))
		for i, e := range events {
			got[i] = e.ID
		}
		t.Fatalf("window [12:30, 13:30) expressed in %s returned %v, want [inside]", east, got)
	}
}

// A poller passes the timestamp of the newest event it already holds, so that
// event must not come back on the next poll. The range bound is the opposite:
// a window starting at midnight contains an event at midnight.
func TestQueryEventsFiltered_SinceIsExclusiveAfterIsInclusive(t *testing.T) {
	db := newTestDB(t)

	ts := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	if err := db.SaveEvent(makeEvent("boundary", "cam", "person", 0.9, ts)); err != nil {
		t.Fatalf("SaveEvent: %v", err)
	}

	since, err := db.QueryEventsFiltered(EventFilters{Since: ts}, 10, 0)
	if err != nil {
		t.Fatalf("QueryEventsFiltered(Since): %v", err)
	}
	if len(since) != 0 {
		t.Errorf("Since=%s returned the event at exactly that timestamp: a poller re-receives its newest event forever", ts)
	}

	after, err := db.QueryEventsFiltered(EventFilters{After: ts}, 10, 0)
	if err != nil {
		t.Fatalf("QueryEventsFiltered(After): %v", err)
	}
	if len(after) != 1 {
		t.Errorf("After=%s returned %d events, want 1: a range starting at midnight must contain an event at midnight", ts, len(after))
	}
}
