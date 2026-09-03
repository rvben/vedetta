package storage

import (
	"testing"
	"time"
)

// The kinds used below are the recorder's vocabulary, not storage's. This layer
// deliberately knows nothing about what makes a failure permanent; it is told.
const (
	testKindPermanent = "source_undecodable"
	testKindTransient = "worker_exit"
)

func segmentFailures(t *testing.T, db *DB, id int64) (int, string) {
	t.Helper()
	var failures int
	var kind string
	if err := db.QueryRowForTest(
		"SELECT recompress_failures, recompress_failure_kind FROM segments WHERE id = ?", id,
	).Scan(&failures, &kind); err != nil {
		t.Fatalf("read segment %d: %v", id, err)
	}
	return failures, kind
}

func clipFailures(t *testing.T, db *DB, id string) (int, string) {
	t.Helper()
	var failures int
	var kind string
	if err := db.QueryRowForTest(
		"SELECT recompress_failures, recompress_failure_kind FROM events WHERE id = ?", id,
	).Scan(&failures, &kind); err != nil {
		t.Fatalf("read event %s: %v", id, err)
	}
	return failures, kind
}

// TestIncrementSegmentRecompressFailures_RecordsTheLatestKind checks that the
// counter carries why it was raised. A bare count cannot distinguish a segment
// waiting on a codec from one whose video no decoder will ever accept, and the
// startup reset needs that distinction to avoid re-running hopeless work at
// every restart forever.
func TestIncrementSegmentRecompressFailures_RecordsTheLatestKind(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	id := saveTestSegment(t, db, "cam1", "/tmp/seg.mp4", now.Add(-48*time.Hour), now.Add(-47*time.Hour), 1000)

	if err := db.IncrementSegmentRecompressFailures(id, testKindTransient); err != nil {
		t.Fatalf("increment: %v", err)
	}
	if failures, kind := segmentFailures(t, db, id); failures != 1 || kind != testKindTransient {
		t.Fatalf("after first failure: failures=%d kind=%q, want 1/%q", failures, kind, testKindTransient)
	}

	// The most recent cause wins. A file that failed transiently and then
	// turned out to be undecodable is undecodable.
	if err := db.IncrementSegmentRecompressFailures(id, testKindPermanent); err != nil {
		t.Fatalf("increment: %v", err)
	}
	failures, kind := segmentFailures(t, db, id)
	if failures != 2 {
		t.Errorf("failures = %d, want 2", failures)
	}
	if kind != testKindPermanent {
		t.Errorf("kind = %q, want %q: the newest cause describes the file now", kind, testKindPermanent)
	}
}

// TestResetStuckRecompressFailures_SkipsPermanentKinds is the point of the
// whole change. Production retried the same 18 unrecompressable files exactly
// 25 times each, once per restart, because the reset cleared every capped
// segment unconditionally. Files whose last failure was permanent must stay
// retired; everything else must still be freed, which is what the reset exists
// for.
func TestResetStuckRecompressFailures_SkipsPermanentKinds(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()

	cap3 := func(path, kind string) int64 {
		t.Helper()
		id := saveTestSegment(t, db, "cam1", path, now.Add(-48*time.Hour), now.Add(-47*time.Hour), 1000)
		for range 3 {
			if err := db.IncrementSegmentRecompressFailures(id, kind); err != nil {
				t.Fatalf("increment %s: %v", path, err)
			}
		}
		return id
	}

	permanent := cap3("/tmp/undecodable.mp4", testKindPermanent)
	transient := cap3("/tmp/transient.mp4", testKindTransient)
	// A row capped before the column existed carries no kind. An unknown
	// cause is not evidence a file is beyond help, so it must still reset.
	legacy := cap3("/tmp/legacy.mp4", "")

	n, err := db.ResetStuckRecompressFailures([]string{testKindPermanent, "source_not_fragmented"})
	if err != nil {
		t.Fatalf("ResetStuckRecompressFailures: %v", err)
	}
	if n != 2 {
		t.Errorf("reset %d rows, want 2 (the transient and the unclassified one)", n)
	}

	if failures, _ := segmentFailures(t, db, permanent); failures != 3 {
		t.Errorf("permanently failed segment reset to %d failures, want it left at 3", failures)
	}
	if failures, _ := segmentFailures(t, db, transient); failures != 0 {
		t.Errorf("transiently failed segment left at %d failures, want 0", failures)
	}
	if failures, _ := segmentFailures(t, db, legacy); failures != 0 {
		t.Errorf("unclassified segment left at %d failures, want 0", failures)
	}

	// The retired segment must actually stay out of the work queue, not just
	// hold a number.
	segs, err := db.GetSegmentsForRecompression("cam1", now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("GetSegmentsForRecompression: %v", err)
	}
	for _, s := range segs {
		if s.ID == permanent {
			t.Error("permanently failed segment is queued for recompression again")
		}
	}
}

// TestResetStuckRecompressFailures_NoPermanentKindsResetsEverything pins the
// empty-list case. It is the caller's way of saying "nothing is permanent", and
// it also has to survive being spliced into SQL, where an empty IN () list is a
// syntax error rather than a match against nothing.
func TestResetStuckRecompressFailures_NoPermanentKindsResetsEverything(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	id := saveTestSegment(t, db, "cam1", "/tmp/seg.mp4", now.Add(-48*time.Hour), now.Add(-47*time.Hour), 1000)
	for range 3 {
		if err := db.IncrementSegmentRecompressFailures(id, testKindPermanent); err != nil {
			t.Fatalf("increment: %v", err)
		}
	}

	n, err := db.ResetStuckRecompressFailures(nil)
	if err != nil {
		t.Fatalf("ResetStuckRecompressFailures(nil): %v", err)
	}
	if n != 1 {
		t.Errorf("reset %d rows, want 1", n)
	}
	if failures, _ := segmentFailures(t, db, id); failures != 0 {
		t.Errorf("failures = %d, want 0", failures)
	}
}

// TestResetStuckClipRecompressFailures_SkipsPermanentKinds is the clip half of
// the same rule. Clips and segments are separate tables with separate counters,
// so a fix applied to one leaves the other looping.
func TestResetStuckClipRecompressFailures_SkipsPermanentKinds(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()

	cap3 := func(id, kind string) {
		t.Helper()
		mustClipEvent(t, db, id, "cam1", now.Add(-48*time.Hour), 1000)
		for range 3 {
			if err := db.IncrementClipRecompressFailures(id, kind); err != nil {
				t.Fatalf("increment %s: %v", id, err)
			}
		}
	}

	cap3("clip-permanent", testKindPermanent)
	cap3("clip-transient", testKindTransient)
	cap3("clip-legacy", "")

	n, err := db.ResetStuckClipRecompressFailures([]string{testKindPermanent, "source_not_fragmented"})
	if err != nil {
		t.Fatalf("ResetStuckClipRecompressFailures: %v", err)
	}
	if n != 2 {
		t.Errorf("reset %d rows, want 2", n)
	}

	if failures, _ := clipFailures(t, db, "clip-permanent"); failures != 3 {
		t.Errorf("permanently failed clip reset to %d failures, want it left at 3", failures)
	}
	if failures, _ := clipFailures(t, db, "clip-transient"); failures != 0 {
		t.Errorf("transiently failed clip left at %d failures, want 0", failures)
	}
	if failures, _ := clipFailures(t, db, "clip-legacy"); failures != 0 {
		t.Errorf("unclassified clip left at %d failures, want 0", failures)
	}

	clips, err := db.GetClipsForRecompression("cam1", now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("GetClipsForRecompression: %v", err)
	}
	for _, c := range clips {
		if c.EventID == "clip-permanent" {
			t.Error("permanently failed clip is queued for recompression again")
		}
	}
}
