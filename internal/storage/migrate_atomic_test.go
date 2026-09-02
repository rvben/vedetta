package storage

import (
	"database/sql"
	"strings"
	"testing"
)

// seedV1WithStaleTimestamps builds a database at schema version 1 holding one
// segment and one event whose timestamps are in the RFC3339 form that the
// version 2 step rewrites. Both are stale, so the step has work to do on two
// different tables.
func seedV1WithStaleTimestamps(t *testing.T) *sql.DB {
	t.Helper()
	db, _ := openRaw(t)

	if _, err := db.Exec(baselineSchema); err != nil {
		t.Fatalf("baseline schema: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatalf("stamp version 1: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO segments (camera, path, start_time, end_time, size_bytes)
		VALUES ('front', '/rec/front/a.mp4', '2026-01-02T03:04:05Z', '2026-01-02T03:14:05Z', 10)`); err != nil {
		t.Fatalf("insert segment: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO events (id, camera, label, score, timestamp)
		VALUES ('evt-1', 'front', 'person', 0.9, '2026-01-02T03:05:00Z')`); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	return db
}

// rawTimestamp reads the bytes actually stored in a column, which is what the
// migration decides on. Scanning into a time.Time would let the driver reformat
// the value and hide the stored representation.
func rawTimestamp(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	var raw string
	if err := db.QueryRow(query).Scan(&raw); err != nil {
		t.Fatalf("read timestamp: %v", err)
	}
	return raw
}

// TestMigrate_FailedStepDoesNotAdvanceVersion is the case a half-applied
// migration used to survive: a row rewrite fails, the failure is swallowed, and
// the schema version advances anyway, so the step never runs again and the
// database keeps timestamps in a format that bare comparisons cannot order.
//
// The trigger fails the events rewrite. The segments rewrite runs first in the
// same step, so it also proves the step's earlier work is rolled back rather
// than left behind.
func TestMigrate_FailedStepDoesNotAdvanceVersion(t *testing.T) {
	db := seedV1WithStaleTimestamps(t)

	if _, err := db.Exec(`CREATE TRIGGER block_event_ts BEFORE UPDATE OF timestamp ON events
		BEGIN SELECT RAISE(ABORT, 'simulated write failure'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	err := migrate(db)
	if err == nil {
		t.Fatal("migrate reported success while a row rewrite failed")
	}
	if !strings.Contains(err.Error(), "simulated write failure") {
		t.Errorf("error must carry the underlying failure, got: %v", err)
	}

	if got := mustUserVersion(t, db); got != 1 {
		t.Errorf("user_version = %d after a failed step, want 1: the database is stamped as migrated but is not", got)
	}

	segRaw := rawTimestamp(t, db, "SELECT CAST(start_time AS TEXT) FROM segments")
	if segRaw != "2026-01-02T03:04:05Z" {
		t.Errorf("segments.start_time = %q, want the original value: the failed step left partial work behind", segRaw)
	}
	evtRaw := rawTimestamp(t, db, "SELECT CAST(timestamp AS TEXT) FROM events")
	if evtRaw != "2026-01-02T03:05:00Z" {
		t.Errorf("events.timestamp = %q, want the original value", evtRaw)
	}
}

// TestMigrate_RetriesAfterAFailedStep covers the point of not advancing the
// version: once the cause is gone the next start completes the same step.
func TestMigrate_RetriesAfterAFailedStep(t *testing.T) {
	db := seedV1WithStaleTimestamps(t)

	if _, err := db.Exec(`CREATE TRIGGER block_event_ts BEFORE UPDATE OF timestamp ON events
		BEGIN SELECT RAISE(ABORT, 'simulated write failure'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	if err := migrate(db); err == nil {
		t.Fatal("migrate reported success while a row rewrite failed")
	}
	if _, err := db.Exec(`DROP TRIGGER block_event_ts`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}

	if err := migrate(db); err != nil {
		t.Fatalf("migrate after the failure was cleared: %v", err)
	}
	if got := mustUserVersion(t, db); got != currentSchemaVersion {
		t.Errorf("user_version = %d, want %d", got, currentSchemaVersion)
	}
	for _, q := range []string{
		"SELECT CAST(start_time AS TEXT) FROM segments",
		"SELECT CAST(end_time AS TEXT) FROM segments",
		"SELECT CAST(timestamp AS TEXT) FROM events",
	} {
		if raw := rawTimestamp(t, db, q); !isCanonicalTimestamp(raw) {
			t.Errorf("%s = %q, want a canonical timestamp", q, raw)
		}
	}

	// A completed migration stays completed.
	if err := migrate(db); err != nil {
		t.Fatalf("migrate is not idempotent after a retry: %v", err)
	}
	if got := mustUserVersion(t, db); got != currentSchemaVersion {
		t.Errorf("user_version = %d after a repeat run, want %d", got, currentSchemaVersion)
	}
}

// TestMigrate_UnparseableTimestampIsSkippedNotFatal separates the two kinds of
// row problem. A value no layout can read is a permanent property of the data:
// the step counts it, leaves it alone, and still completes, because failing
// forever would keep the server from starting.
func TestMigrate_UnparseableTimestampIsSkippedNotFatal(t *testing.T) {
	db := seedV1WithStaleTimestamps(t)

	if _, err := db.Exec(`INSERT INTO segments (camera, path, start_time, end_time, size_bytes)
		VALUES ('front', '/rec/front/b.mp4', 'not a timestamp', '2026-01-02T03:24:05Z', 10)`); err != nil {
		t.Fatalf("insert unparseable segment: %v", err)
	}

	if err := migrate(db); err != nil {
		t.Fatalf("migrate must not fail on one unreadable value: %v", err)
	}
	if got := mustUserVersion(t, db); got != currentSchemaVersion {
		t.Errorf("user_version = %d, want %d", got, currentSchemaVersion)
	}

	raw := rawTimestamp(t, db, "SELECT CAST(start_time AS TEXT) FROM segments WHERE path = '/rec/front/b.mp4'")
	if raw != "not a timestamp" {
		t.Errorf("start_time = %q, want the original value: an unreadable timestamp must not be replaced by an invented one", raw)
	}
	good := rawTimestamp(t, db, "SELECT CAST(start_time AS TEXT) FROM segments WHERE path = '/rec/front/a.mp4'")
	if !isCanonicalTimestamp(good) {
		t.Errorf("start_time = %q, want the readable row rewritten", good)
	}
}

// TestMigrationStepsCoverEverySchemaVersion guards the table itself: a version
// with no step would leave an upgraded database stamped below what this build
// expects, and a step numbered above currentSchemaVersion would run on every
// start.
func TestMigrationStepsCoverEverySchemaVersion(t *testing.T) {
	if len(migrationSteps) != currentSchemaVersion {
		t.Fatalf("%d migration steps for schema version %d", len(migrationSteps), currentSchemaVersion)
	}
	for i, step := range migrationSteps {
		if step.version != i+1 {
			t.Errorf("migrationSteps[%d].version = %d, want %d", i, step.version, i+1)
		}
		if step.name == "" {
			t.Errorf("migrationSteps[%d] has no name", i)
		}
		if step.run == nil {
			t.Errorf("migrationSteps[%d] has no run func", i)
		}
	}
}
