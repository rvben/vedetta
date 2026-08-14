package storage

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/camera"
)

// openRaw opens a raw *sql.DB on a temp file without running migrate, so tests
// can construct legacy schemas and drive migrate directly.
func openRaw(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "raw.db")
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func mustUserVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return v
}

// TestMigrate_FreshDBStampsCurrentVersion verifies a brand-new database is
// stamped with the current schema version after migration.
func TestMigrate_FreshDBStampsCurrentVersion(t *testing.T) {
	db, _ := openRaw(t)

	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if got := mustUserVersion(t, db); got != currentSchemaVersion {
		t.Errorf("user_version = %d, want %d", got, currentSchemaVersion)
	}
}

// TestMigrate_Idempotent verifies running migrate repeatedly on the same DB is
// a no-op that neither errors nor changes the version.
func TestMigrate_Idempotent(t *testing.T) {
	db, _ := openRaw(t)

	for i := 0; i < 3; i++ {
		if err := migrate(db); err != nil {
			t.Fatalf("migrate pass %d: %v", i, err)
		}
	}
	if got := mustUserVersion(t, db); got != currentSchemaVersion {
		t.Errorf("user_version = %d, want %d", got, currentSchemaVersion)
	}
}

// TestMigrate_UpgradesLegacySchema simulates a database created before column
// versioning: a minimal events table missing every later-added column. After
// migrate the columns must exist, the version must be stamped, and writes must
// succeed.
func TestMigrate_UpgradesLegacySchema(t *testing.T) {
	db, _ := openRaw(t)

	// Pre-versioning events table: only the original columns.
	if _, err := db.Exec(`
		CREATE TABLE events (
			id TEXT PRIMARY KEY,
			camera TEXT NOT NULL,
			label TEXT NOT NULL,
			score REAL NOT NULL,
			box_x1 INTEGER, box_y1 INTEGER, box_x2 INTEGER, box_y2 INTEGER,
			timestamp DATETIME NOT NULL,
			snapshot_path TEXT,
			clip_path TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`); err != nil {
		t.Fatal(err)
	}

	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	for _, col := range []string{"end_time", "zone_name", "snapshot_available", "clip_available", "object_name", "sub_label"} {
		exists, err := columnExists(db, "events", col)
		if err != nil {
			t.Fatalf("columnExists(%s): %v", col, err)
		}
		if !exists {
			t.Errorf("expected legacy upgrade to add events.%s", col)
		}
	}

	for _, col := range []string{"recompressed", "recompressed_at", "recompress_failures", "clip_size_bytes"} {
		exists, err := columnExists(db, "events", col)
		if err != nil {
			t.Fatalf("columnExists(%s): %v", col, err)
		}
		if !exists {
			t.Errorf("expected v0->v3 upgrade to add events.%s", col)
		}
	}

	if got := mustUserVersion(t, db); got != currentSchemaVersion {
		t.Errorf("user_version = %d, want %d", got, currentSchemaVersion)
	}

	// A normal write must work against the upgraded schema.
	wrapped := &DB{db: db}
	if err := wrapped.SaveEvent(camera.Event{
		ID:         "e1",
		CameraName: "cam1",
		Label:      "person",
		Score:      0.9,
		Box:        [4]int{1, 2, 3, 4},
		Timestamp:  time.Now(),
	}); err != nil {
		t.Fatalf("SaveEvent after upgrade: %v", err)
	}
}

// TestMigrate_ConcurrentLegacyUpgrade reproduces two processes migrating the
// same pre-versioned database at once. Both can read user_version=0 and observe
// a legacy column as missing before either ALTER commits; the loser must
// tolerate the resulting duplicate-column error rather than fail startup.
func TestMigrate_ConcurrentLegacyUpgrade(t *testing.T) {
	seed, path := openRaw(t)
	// Legacy events table missing every later-added column, so all 13
	// backfill ALTERs contend.
	if _, err := seed.Exec(`
		CREATE TABLE events (
			id TEXT PRIMARY KEY,
			camera TEXT NOT NULL,
			label TEXT NOT NULL,
			score REAL NOT NULL,
			box_x1 INTEGER, box_y1 INTEGER, box_x2 INTEGER, box_y2 INTEGER,
			timestamp DATETIME NOT NULL,
			snapshot_path TEXT,
			clip_path TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`); err != nil {
		t.Fatal(err)
	}

	// Each worker opens its own *sql.DB (its own connection pool) on the same
	// file, mirroring separate processes: WAL lets them all read table_info
	// concurrently and observe the same missing columns before any ALTER wins.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"

	const workers = 8
	errs := make(chan error, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		go func() {
			conn, err := sql.Open("sqlite", dsn)
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = conn.Close() }()
			<-start // align goroutines to maximize the check-then-alter race
			errs <- migrate(conn)
		}()
	}
	close(start)
	for i := 0; i < workers; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent migrate failed: %v", err)
		}
	}

	if got := mustUserVersion(t, seed); got != currentSchemaVersion {
		t.Errorf("user_version = %d, want %d", got, currentSchemaVersion)
	}
}

// TestMigrate_V2CanonicalizesLegacyRFC3339 simulates a database that reached
// version 1 while still holding timestamps in RFC3339 ("T"-separated) form -
// rows that version 1's needsNormalization check does not match because they
// carry no "+offset"/monotonic marker. The version 2 migration must rewrite
// every timestamp column into the canonical driver format (space-separated, no
// "T") so that index-friendly bare comparisons are correct.
func TestMigrate_V2CanonicalizesLegacyRFC3339(t *testing.T) {
	db, _ := openRaw(t)

	// Build the baseline schema, then mark the DB as already at version 1 so the
	// version 0 backfill/normalize path is skipped and only the v2 step runs.
	if err := migrate(db); err != nil {
		t.Fatalf("initial migrate: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}

	// Insert RFC3339 "T"-format rows as raw text (bypassing the driver's
	// time.Time serialization) to mimic legacy data the v1 path missed.
	if _, err := db.Exec(
		`INSERT INTO segments (camera, path, start_time, end_time, size_bytes) VALUES (?, ?, ?, ?, ?)`,
		"cam1", "cam1/2024/seg.mp4", "2024-01-02T03:04:05Z", "2024-01-02T03:05:05Z", 100,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO events (id, camera, label, score, timestamp, end_time) VALUES (?, ?, ?, ?, ?, ?)`,
		"e1", "cam1", "person", 0.9, "2024-01-02T03:04:05Z", "2024-01-02T03:05:05Z",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO motion_activity (camera, bucket, score) VALUES (?, ?, ?)`,
		"cam1", "2024-01-02T03:04:00Z", 0.5,
	); err != nil {
		t.Fatal(err)
	}
	// object_sightings.timestamp is ordered by ListObjectSightings;
	// auth_sessions.expires_at is text-compared by DeleteExpiredSessions;
	// api_tokens.created_at is ordered by ListAPITokensByUser. All three are
	// written via utc() in production, so legacy "T" rows must be canonicalized.
	if _, err := db.Exec(
		`INSERT INTO known_objects (id, name, label) VALUES (?, ?, ?)`,
		1, "car1", "car",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO object_sightings (event_id, camera, object_id, similarity, timestamp) VALUES (?, ?, ?, ?, ?)`,
		"e1", "cam1", 1, 0.5, "2024-01-02T03:04:05Z",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO auth_sessions (id, username, csrf_token, created_at, last_seen_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"s1", "admin", "csrf", "2024-01-02T03:04:05Z", "2024-01-02T03:04:05Z", "2024-01-02T04:04:05Z",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO api_tokens (username, name, token_prefix, token_hash, created_at) VALUES (?, ?, ?, ?, ?)`,
		"admin", "tok", "abcd", []byte("hash"), "2024-01-02T03:04:05Z",
	); err != nil {
		t.Fatal(err)
	}
	// object_references.created_at and storage_audit.ts are both written via
	// utc() (SaveObjectReference, InsertStorageAudit) - they do not rely on the
	// CURRENT_TIMESTAMP default - and are ordered as text (ListObjectReferences
	// ORDER BY created_at, StorageAudit ORDER BY ts DESC).
	if _, err := db.Exec(
		`INSERT INTO object_references (object_id, embedding, created_at) VALUES (?, ?, ?)`,
		1, []byte("emb"), "2024-01-02T03:04:05Z",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO storage_audit (ts, actor, scope_json, bytes_freed, file_count) VALUES (?, ?, ?, ?, ?)`,
		"2024-01-02T03:04:05Z", "admin", "{}", 0, 0,
	); err != nil {
		t.Fatal(err)
	}

	if err := migrate(db); err != nil {
		t.Fatalf("v2 migrate: %v", err)
	}

	if got := mustUserVersion(t, db); got != currentSchemaVersion {
		t.Errorf("user_version = %d, want %d", got, currentSchemaVersion)
	}

	// After migration no on-disk timestamp may retain the "T" separator: a bare
	// string comparison against a canonical (space-separated) parameter would
	// otherwise be wrong. CAST(... AS TEXT) reveals the true stored bytes, which
	// the driver would otherwise reformat when scanning into a string.
	for _, q := range []string{
		"SELECT CAST(start_time AS TEXT) FROM segments",
		"SELECT CAST(end_time AS TEXT) FROM segments",
		"SELECT CAST(timestamp AS TEXT) FROM events",
		"SELECT CAST(end_time AS TEXT) FROM events",
		"SELECT CAST(bucket AS TEXT) FROM motion_activity",
		"SELECT CAST(timestamp AS TEXT) FROM object_sightings",
		"SELECT CAST(expires_at AS TEXT) FROM auth_sessions",
		"SELECT CAST(created_at AS TEXT) FROM api_tokens",
		"SELECT CAST(created_at AS TEXT) FROM object_references",
		"SELECT CAST(ts AS TEXT) FROM storage_audit",
	} {
		var raw string
		if err := db.QueryRow(q).Scan(&raw); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		// Canonical form separates date and time with a space at index 10; an
		// RFC3339 value has a "T" there. (The "UTC" suffix also contains a "T",
		// so a substring search would give a false positive.)
		if len(raw) <= 10 || raw[10] != ' ' {
			t.Errorf("%s still holds non-canonical %q after v2 migration", q, raw)
		}
		if !strings.HasSuffix(raw, " +0000 UTC") {
			t.Errorf("%s = %q, expected canonical UTC form", q, raw)
		}
	}
}

func TestMigrate_V6RestoresOnlyUnambiguousObjectSightingLinks(t *testing.T) {
	db, _ := openRaw(t)
	if err := migrate(db); err != nil {
		t.Fatalf("initial migrate: %v", err)
	}
	if err := setUserVersion(db, 5); err != nil {
		t.Fatal(err)
	}

	ts := utc(time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC))
	for _, row := range []struct {
		id    int
		name  string
		label string
	}{
		{1, "Family bike", "bicycle"},
		{2, "Expired car", "car"},
		{3, "Ambiguous person", "person"},
	} {
		if _, err := db.Exec(`INSERT INTO known_objects (id, name, label) VALUES (?, ?, ?)`, row.id, row.name, row.label); err != nil {
			t.Fatal(err)
		}
	}

	for _, row := range []struct {
		id, camera, label, name string
	}{
		{"bike-event", "garage", "bicycle", "Family bike"},
		{"person-event-a", "front", "person", "Ambiguous person"},
		{"person-event-b", "front", "person", "Ambiguous person"},
	} {
		if _, err := db.Exec(`
			INSERT INTO events (id, camera, label, score, timestamp, object_name)
			VALUES (?, ?, ?, 0.9, ?, ?)`, row.id, row.camera, row.label, ts, row.name); err != nil {
			t.Fatal(err)
		}
	}

	for _, row := range []struct {
		id, objectID int
		camera       string
	}{
		{1, 1, "garage"},
		{2, 2, "driveway"},
		{3, 3, "front"},
	} {
		if _, err := db.Exec(`
			INSERT INTO object_sightings (id, event_id, camera, object_id, similarity, timestamp)
			VALUES (?, NULL, ?, ?, 0.8, ?)`, row.id, row.camera, row.objectID, ts); err != nil {
			t.Fatal(err)
		}
	}

	if err := migrate(db); err != nil {
		t.Fatalf("v6 migrate: %v", err)
	}

	wants := map[int]sql.NullString{
		1: {String: "bike-event", Valid: true},
		2: {}, // source event expired or was never retained
		3: {}, // two exact candidates: never guess
	}
	for id, want := range wants {
		var got sql.NullString
		if err := db.QueryRow(`SELECT event_id FROM object_sightings WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("sighting %d: %v", id, err)
		}
		if got != want {
			t.Errorf("sighting %d event_id = %+v, want %+v", id, got, want)
		}
	}
	if got := mustUserVersion(t, db); got != currentSchemaVersion {
		t.Errorf("user_version = %d, want %d", got, currentSchemaVersion)
	}
}

// TestColumnExists verifies detection of present and absent columns.
func TestColumnExists(t *testing.T) {
	db, _ := openRaw(t)
	if _, err := db.Exec(`CREATE TABLE t (a INTEGER, b TEXT)`); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		col  string
		want bool
	}{
		{"a", true},
		{"b", true},
		{"c", false},
	} {
		got, err := columnExists(db, "t", tc.col)
		if err != nil {
			t.Fatalf("columnExists(%q): %v", tc.col, err)
		}
		if got != tc.want {
			t.Errorf("columnExists(%q) = %v, want %v", tc.col, got, tc.want)
		}
	}
}

// TestMigrate_FreshDBHasClipRecompressColumns verifies baselineSchema defines
// the clip recompression columns on a brand-new database.
func TestMigrate_FreshDBHasClipRecompressColumns(t *testing.T) {
	db, _ := openRaw(t)
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, col := range []string{"recompressed", "recompressed_at", "recompress_failures", "clip_size_bytes"} {
		exists, err := columnExists(db, "events", col)
		if err != nil {
			t.Fatalf("columnExists(%s): %v", col, err)
		}
		if !exists {
			t.Errorf("fresh DB missing events.%s", col)
		}
	}
}

// TestMigrate_V2UpgradesToClipRecompress simulates a v2 database (current
// baseline without the clip recompression columns) and verifies the v3 block
// adds them and stamps the new version.
func TestMigrate_V2UpgradesToClipRecompress(t *testing.T) {
	db, _ := openRaw(t)

	// A v2 events table: every column the v2 baseline had, but none of the
	// v3 clip-recompression columns.
	if _, err := db.Exec(`
		CREATE TABLE events (
			id TEXT PRIMARY KEY,
			camera TEXT NOT NULL,
			label TEXT NOT NULL,
			score REAL NOT NULL,
			box_x1 INTEGER, box_y1 INTEGER, box_x2 INTEGER, box_y2 INTEGER,
			timestamp DATETIME NOT NULL,
			end_time DATETIME,
			snapshot_path TEXT,
			snapshot_available BOOLEAN NOT NULL DEFAULT 0,
			clip_path TEXT,
			clip_available BOOLEAN NOT NULL DEFAULT 0,
			zone_name TEXT,
			object_name TEXT,
			sub_label TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`); err != nil {
		t.Fatal(err)
	}
	if err := setUserVersion(db, 2); err != nil {
		t.Fatal(err)
	}

	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	for _, col := range []string{"recompressed", "recompressed_at", "recompress_failures", "clip_size_bytes"} {
		exists, err := columnExists(db, "events", col)
		if err != nil {
			t.Fatalf("columnExists(%s): %v", col, err)
		}
		if !exists {
			t.Errorf("v2->v3 upgrade missing events.%s", col)
		}
	}
	if got := mustUserVersion(t, db); got != currentSchemaVersion {
		t.Errorf("user_version = %d, want %d", got, currentSchemaVersion)
	}
	// Re-running migrate must be a no-op.
	if err := migrate(db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	// A normal write must work against the upgraded schema.
	wrapped := &DB{db: db}
	if err := wrapped.SaveEvent(camera.Event{
		ID:         "e1",
		CameraName: "cam1",
		Label:      "person",
		Score:      0.9,
		Box:        [4]int{1, 2, 3, 4},
		Timestamp:  time.Now(),
	}); err != nil {
		t.Fatalf("SaveEvent after v2->v3 upgrade: %v", err)
	}
}

// TestEnsureColumn_SurfacesRealErrors verifies the helper no longer swallows
// genuine failures: adding a column to a non-existent table must error rather
// than be silently ignored. Adding an already-present column is a clean no-op.
func TestEnsureColumn_SurfacesRealErrors(t *testing.T) {
	db, _ := openRaw(t)
	if _, err := db.Exec(`CREATE TABLE t (a INTEGER)`); err != nil {
		t.Fatal(err)
	}

	// Already-present column: no-op, no error.
	if err := ensureColumn(db, "t", "a", "ALTER TABLE t ADD COLUMN a INTEGER"); err != nil {
		t.Errorf("ensureColumn on existing column should be a no-op, got %v", err)
	}

	// Missing column on an existing table: applied cleanly.
	if err := ensureColumn(db, "t", "b", "ALTER TABLE t ADD COLUMN b TEXT"); err != nil {
		t.Errorf("ensureColumn adding a new column: %v", err)
	}
	exists, err := columnExists(db, "t", "b")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Error("expected column b to be added")
	}

	// Non-existent table: must surface the error, not swallow it.
	if err := ensureColumn(db, "missing_table", "x", "ALTER TABLE missing_table ADD COLUMN x TEXT"); err == nil {
		t.Error("ensureColumn against a non-existent table must return an error")
	}
}
