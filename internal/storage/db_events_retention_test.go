package storage

import (
	"fmt"
	"testing"
	"time"
)

// seedEvents inserts n events for one camera, one minute apart, ending at
// newest. The inserts share a transaction and skip the event API on purpose:
// this test is about how many rows a delete removes per transaction, not about
// how events are created.
func seedEvents(t *testing.T, d *DB, cameraName string, n int, newest time.Time) []string {
	t.Helper()
	tx, err := d.db.Begin()
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%s-%05d", cameraName, i)
		ts := newest.Add(-time.Duration(n-1-i) * time.Minute)
		if _, err := tx.Exec(`
			INSERT INTO events (id, camera, label, score, timestamp)
			VALUES (?, ?, 'person', 0.9, ?)`, id, cameraName, utc(ts)); err != nil {
			t.Fatalf("insert seed event %s: %v", id, err)
		}
		ids = append(ids, id)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	return ids
}

// countEventsBefore reports how many events are still older than cutoff.
func countEventsBefore(t *testing.T, d *DB, cutoff time.Time) int {
	t.Helper()
	var n int
	if err := d.db.QueryRow("SELECT COUNT(*) FROM events WHERE timestamp < ?", utc(cutoff)).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

// TestDeleteEventsOlderThan_CommitsWorkBeforeAFailure pins the delete to bounded
// transactions. One transaction covering every expired event is all or nothing:
// a failure on the last row throws away the whole night's cleanup, and the next
// run starts over on the same rows. Bounded batches commit as they go, so a
// failure costs one batch.
//
// A trigger fails the delete of the newest expired event, which is the last one
// any batch reaches, so every earlier batch must already be committed.
func TestDeleteEventsOlderThan_CommitsWorkBeforeAFailure(t *testing.T) {
	d := newTestDB(t)
	const seeded = 1000
	newest := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	ids := seedEvents(t, d, "front", seeded, newest)
	cutoff := newest.Add(time.Minute)

	// A trigger body cannot carry bind parameters, so the id is written into the
	// statement. It is a fixed test-generated value, not input.
	trigger := fmt.Sprintf(`
		CREATE TRIGGER block_last_event BEFORE DELETE ON events
		WHEN OLD.id = '%s'
		BEGIN SELECT RAISE(ABORT, 'simulated delete failure'); END`, ids[len(ids)-1])
	if _, err := d.db.Exec(trigger); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	err := d.DeleteEventsOlderThan(cutoff)
	if err == nil {
		t.Fatal("DeleteEventsOlderThan reported success while a row delete failed")
	}

	remaining := countEventsBefore(t, d, cutoff)
	if remaining == seeded {
		t.Fatalf("all %d expired events survived the failure: no batch committed, so the delete runs as one unbounded transaction (or the batch size exceeds the %d seeded events)", seeded, seeded)
	}
	if remaining == 0 {
		t.Fatalf("every expired event was deleted, including the one the trigger rejects")
	}
}

// eventIDsInDB returns the ids still stored, oldest first.
func eventIDsInDB(t *testing.T, d *DB) []string {
	t.Helper()
	rows, err := d.db.Query("SELECT id FROM events ORDER BY timestamp")
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan event id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read events: %v", err)
	}
	return ids
}

func countRows(t *testing.T, d *DB, query string) int {
	t.Helper()
	var n int
	if err := d.db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// TestDeleteEventsOlderThan_AcrossBatches runs the delete over several batches
// and checks the outcome is the one a single transaction would have produced:
// every expired event gone, every newer event untouched, the activities that
// only held expired events removed, an activity that straddles the cutoff kept
// and resummarized, and a count that matches the rows actually removed.
func TestDeleteEventsOlderThan_AcrossBatches(t *testing.T) {
	d := newTestDB(t)
	cutoff := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)

	// Ten expired events, five minutes apart so each becomes its own activity.
	var expired []string
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("old-%d", i)
		mustSaveEvent(t, d, makeEvent(id, "front", "person", 0.9, cutoff.Add(-time.Duration(60-5*i)*time.Minute)))
		expired = append(expired, id)
	}
	// One activity holding an event on each side of the cutoff. It must survive
	// the delete and end up describing only the event that is left.
	mustSaveEvent(t, d, makeEvent("straddle-old", "front", "person", 0.9, cutoff.Add(-30*time.Second)))
	mustSaveEvent(t, d, makeEvent("straddle-new", "front", "person", 0.9, cutoff.Add(30*time.Second)))
	expired = append(expired, "straddle-old")
	// Four events well after the cutoff, each its own activity.
	var kept []string
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("new-%d", i)
		mustSaveEvent(t, d, makeEvent(id, "front", "person", 0.9, cutoff.Add(time.Duration(10+5*i)*time.Minute)))
		kept = append(kept, id)
	}
	kept = append(kept, "straddle-new")

	if got := countRows(t, d, "SELECT COUNT(*) FROM activities"); got != 15 {
		t.Fatalf("seeded %d activities, want 15: the events did not group as this test assumes", got)
	}

	// A batch size well below the number of expired events forces several
	// batches: 11 expired events run as 3 + 3 + 3 + 2.
	deleted, err := d.deleteEventsOlderThan(cutoff, 3)
	if err != nil {
		t.Fatalf("deleteEventsOlderThan: %v", err)
	}
	if deleted != int64(len(expired)) {
		t.Errorf("deleted count = %d, want %d", deleted, len(expired))
	}

	got := eventIDsInDB(t, d)
	if len(got) != len(kept) {
		t.Fatalf("events left = %v, want %v", got, kept)
	}
	left := make(map[string]bool, len(got))
	for _, id := range got {
		left[id] = true
	}
	for _, id := range kept {
		if !left[id] {
			t.Errorf("event %s was deleted but is newer than the cutoff", id)
		}
	}
	for _, id := range expired {
		if left[id] {
			t.Errorf("event %s is older than the cutoff and should be gone", id)
		}
	}

	// The ten activities that only held expired events go with them; the four
	// recent ones and the straddling one remain.
	if n := countRows(t, d, "SELECT COUNT(*) FROM activities"); n != 5 {
		t.Errorf("activities left = %d, want 5", n)
	}
	if n := countRows(t, d, "SELECT COUNT(*) FROM activity_events"); n != 5 {
		t.Errorf("activity_events rows = %d, want 5", n)
	}
	var start, end time.Time
	err = d.db.QueryRow(`
		SELECT a.start_time, a.end_time
		FROM activities a
		JOIN activity_events ae ON ae.activity_id = a.id
		WHERE ae.event_id = 'straddle-new'`).Scan(&start, &end)
	if err != nil {
		t.Fatalf("read straddling activity: %v", err)
	}
	if want := utc(cutoff.Add(30 * time.Second)); !start.Equal(want) {
		t.Errorf("straddling activity start = %s, want %s: it still describes the deleted event", start, want)
	}
	if !end.Equal(utc(cutoff.Add(30 * time.Second))) {
		t.Errorf("straddling activity end = %s, want the surviving event's timestamp", end)
	}

	// Running again has nothing left to do and must not report deletions.
	deleted, err = d.deleteEventsOlderThan(cutoff, 3)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if deleted != 0 {
		t.Errorf("second run deleted %d events, want 0", deleted)
	}
	if n := len(eventIDsInDB(t, d)); n != len(kept) {
		t.Errorf("second run left %d events, want %d", n, len(kept))
	}
}

// TestDeleteEventsOlderThan_BatchSizeBoundary covers the batch loop's exit
// condition at the sizes where an off-by-one shows: a batch that exactly fills
// and one that leaves a remainder.
func TestDeleteEventsOlderThan_BatchSizeBoundary(t *testing.T) {
	cutoff := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		seeded    int
		batchSize int
	}{
		{"exact multiple", 6, 3},
		{"remainder", 7, 3},
		{"single batch", 2, 3},
		{"one per batch", 4, 1},
		{"nothing expired", 0, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDB(t)
			if tc.seeded > 0 {
				seedEvents(t, d, "front", tc.seeded, cutoff.Add(-time.Minute))
			}
			seedEvents(t, d, "back", 2, cutoff.Add(time.Hour))

			deleted, err := d.deleteEventsOlderThan(cutoff, tc.batchSize)
			if err != nil {
				t.Fatalf("deleteEventsOlderThan: %v", err)
			}
			if deleted != int64(tc.seeded) {
				t.Errorf("deleted count = %d, want %d", deleted, tc.seeded)
			}
			if n := countEventsBefore(t, d, cutoff); n != 0 {
				t.Errorf("%d expired events left", n)
			}
			if n := len(eventIDsInDB(t, d)); n != 2 {
				t.Errorf("events left = %d, want the 2 that are newer than the cutoff", n)
			}
		})
	}
}
