package storage

import (
	"fmt"
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

func TestCountEventsToday(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour).Add(1 * time.Hour) // 01:00 today
	yesterday := today.Add(-25 * time.Hour)

	mustSaveEvent(t, db, makeEvent("today1", "cam1", "person", 0.9, today))
	mustSaveEvent(t, db, makeEvent("today2", "cam1", "car", 0.8, today.Add(time.Hour)))
	mustSaveEvent(t, db, makeEvent("yesterday1", "cam1", "person", 0.7, yesterday))

	count, err := db.CountEventsToday()
	if err != nil {
		t.Fatalf("CountEventsToday: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
}

func TestUpdateEventClipPath(t *testing.T) {
	db := newTestDB(t)
	ev := makeEvent("clip-test", "cam1", "person", 0.9, time.Now().UTC())
	mustSaveEvent(t, db, ev)

	if err := db.UpdateEventClipPath("clip-test", "/clips/new.mp4"); err != nil {
		t.Fatalf("UpdateEventClipPath: %v", err)
	}

	got, _ := db.GetEventByID("clip-test")
	if got.ClipPath != "/clips/new.mp4" {
		t.Errorf("ClipPath = %q, want %q", got.ClipPath, "/clips/new.mp4")
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

func TestGetSegmentsForDate(t *testing.T) {
	db := newTestDB(t)
	march20 := time.Date(2026, 3, 20, 14, 0, 0, 0, time.UTC)
	march21 := time.Date(2026, 3, 21, 10, 0, 0, 0, time.UTC)

	mustSaveSegment(t, db, makeSegment("cam1", "/seg1.mp4", march20, march20.Add(5*time.Minute), 1000))
	mustSaveSegment(t, db, makeSegment("cam1", "/seg2.mp4", march20.Add(time.Hour), march20.Add(time.Hour+5*time.Minute), 2000))
	mustSaveSegment(t, db, makeSegment("cam1", "/seg3.mp4", march21, march21.Add(5*time.Minute), 3000))

	segs, err := db.GetSegmentsForDate("cam1", march20)
	if err != nil {
		t.Fatalf("GetSegmentsForDate: %v", err)
	}
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want 2", len(segs))
	}
}

func TestGetSegmentsForDate_DifferentCamera(t *testing.T) {
	db := newTestDB(t)
	ts := time.Date(2026, 3, 20, 14, 0, 0, 0, time.UTC)

	mustSaveSegment(t, db, makeSegment("cam1", "/seg1.mp4", ts, ts.Add(5*time.Minute), 1000))
	mustSaveSegment(t, db, makeSegment("cam2", "/seg2.mp4", ts, ts.Add(5*time.Minute), 2000))

	segs, err := db.GetSegmentsForDate("cam1", ts)
	if err != nil {
		t.Fatalf("GetSegmentsForDate: %v", err)
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

	days, err := db.GetRecordingDays("cam1", 2026, 3)
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

	days, err := db.GetRecordingDays("cam1", 2026, 3)
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

	days, err := db.GetRecordingDays("", 2026, 3)
	if err != nil {
		t.Fatalf("GetRecordingDays: %v", err)
	}
	if len(days) != 2 {
		t.Fatalf("got %d days, want 2", len(days))
	}
}

func TestGetRecordingDays_NoData(t *testing.T) {
	db := newTestDB(t)

	days, err := db.GetRecordingDays("cam1", 2026, 6)
	if err != nil {
		t.Fatalf("GetRecordingDays: %v", err)
	}
	if len(days) != 0 {
		t.Errorf("expected empty slice, got %v", days)
	}
}

// --- QueryEventsForDate ---

func TestQueryEventsForDate(t *testing.T) {
	db := newTestDB(t)
	march20_morning := time.Date(2026, 3, 20, 8, 0, 0, 0, time.UTC)
	march20_evening := time.Date(2026, 3, 20, 20, 0, 0, 0, time.UTC)
	march21 := time.Date(2026, 3, 21, 10, 0, 0, 0, time.UTC)

	mustSaveEvent(t, db, makeEvent("e1", "cam1", "person", 0.9, march20_morning))
	mustSaveEvent(t, db, makeEvent("e2", "cam1", "car", 0.8, march20_evening))
	mustSaveEvent(t, db, makeEvent("e3", "cam1", "person", 0.7, march21))

	events, err := db.QueryEventsForDate("cam1", march20_morning)
	if err != nil {
		t.Fatalf("QueryEventsForDate: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
}

func TestQueryEventsForDate_CameraFilter(t *testing.T) {
	db := newTestDB(t)
	ts := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)

	mustSaveEvent(t, db, makeEvent("e1", "cam1", "person", 0.9, ts))
	mustSaveEvent(t, db, makeEvent("e2", "cam2", "person", 0.8, ts))

	events, err := db.QueryEventsForDate("cam1", ts)
	if err != nil {
		t.Fatalf("QueryEventsForDate: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].ID != "e1" {
		t.Errorf("got ID %q, want e1", events[0].ID)
	}
}

func TestQueryEventsForDate_NoEvents(t *testing.T) {
	db := newTestDB(t)
	ts := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)

	events, err := db.QueryEventsForDate("cam1", ts)
	if err != nil {
		t.Fatalf("QueryEventsForDate: %v", err)
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

	result, err := db.CountEventsByLabel()
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

	result, err := db.CountEventsByLabel()
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

	result, err := db.CountEventsByCamera()
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

	count, err := db.CountEvents()
	if err != nil {
		t.Fatalf("CountEvents: %v", err)
	}
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
}

func TestCountEvents_EmptyDB(t *testing.T) {
	db := newTestDB(t)

	count, err := db.CountEvents()
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

func TestQueryEventsForDate_ReturnsEndTime(t *testing.T) {
	db := newTestDB(t)
	ts := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	endTime := ts.Add(60 * time.Second)

	ev := makeEvent("qd-end", "cam1", "person", 0.9, ts)
	ev.EndTime = endTime
	mustSaveEvent(t, db, ev)

	events, err := db.QueryEventsForDate("cam1", ts)
	if err != nil {
		t.Fatalf("QueryEventsForDate: %v", err)
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
	buckets, err := db.GetMotionActivity("cam1", bucket1)
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
	buckets, err := db.GetMotionActivity("cam1", bucket)
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
	buckets, err := db.GetMotionActivity("cam1", old)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 0 {
		t.Errorf("expected 0 buckets after cleanup, got %d", len(buckets))
	}
	buckets, err = db.GetMotionActivity("cam1", recent)
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
