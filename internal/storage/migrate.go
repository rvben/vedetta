package storage

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// currentSchemaVersion is the schema version this build expects. It is stored
// in SQLite's PRAGMA user_version. A database reporting a lower version is
// upgraded by migrate; databases created before versioning report 0.
const currentSchemaVersion = 9

// baselineSchema creates every table and index for a fresh database. It is
// idempotent (CREATE ... IF NOT EXISTS) and a cheap no-op for existing DBs.
const baselineSchema = `
	CREATE TABLE IF NOT EXISTS events (
		id TEXT PRIMARY KEY,
		camera TEXT NOT NULL,
		label TEXT NOT NULL,
		score REAL NOT NULL,
		box_x1 INTEGER,
		box_y1 INTEGER,
		box_x2 INTEGER,
		box_y2 INTEGER,
		timestamp DATETIME NOT NULL,
		end_time DATETIME,
		snapshot_path TEXT,
		snapshot_available BOOLEAN NOT NULL DEFAULT 0,
		clip_path TEXT,
		clip_available BOOLEAN NOT NULL DEFAULT 0,
		zone_name TEXT,
		object_name TEXT,
		sub_label TEXT,
		category TEXT NOT NULL DEFAULT 'alert',
		kind TEXT NOT NULL DEFAULT 'object',
		answered_at DATETIME,
		answered_by TEXT,
		recompressed INTEGER NOT NULL DEFAULT 0,
		recompressed_at DATETIME,
		recompress_failures INTEGER NOT NULL DEFAULT 0,
		clip_size_bytes INTEGER NOT NULL DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_events_camera ON events(camera);
	CREATE INDEX IF NOT EXISTS idx_events_timestamp ON events(timestamp);
	CREATE INDEX IF NOT EXISTS idx_events_label ON events(label);
	CREATE INDEX IF NOT EXISTS idx_events_camera_timestamp ON events(camera, timestamp);

	CREATE TABLE IF NOT EXISTS activities (
		id TEXT PRIMARY KEY,
		camera TEXT NOT NULL,
		start_time DATETIME NOT NULL,
		end_time DATETIME NOT NULL,
		category TEXT NOT NULL DEFAULT 'alert',
		state TEXT NOT NULL DEFAULT 'finalized',
		finalized_at DATETIME,
		notification_queued_at DATETIME,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_activities_camera_time ON activities(camera, start_time DESC);
	CREATE INDEX IF NOT EXISTS idx_activities_time ON activities(start_time DESC);

	CREATE TABLE IF NOT EXISTS activity_events (
		activity_id TEXT NOT NULL REFERENCES activities(id) ON DELETE CASCADE,
		event_id TEXT NOT NULL UNIQUE REFERENCES events(id) ON DELETE CASCADE,
		added_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (activity_id, event_id)
	);
	CREATE INDEX IF NOT EXISTS idx_activity_events_activity ON activity_events(activity_id);

	CREATE TABLE IF NOT EXISTS activity_event_corrections (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		activity_id TEXT NOT NULL REFERENCES activities(id) ON DELETE CASCADE,
		event_id TEXT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
		action TEXT NOT NULL CHECK(action = 'excluded'),
		reason TEXT NOT NULL DEFAULT '',
		actor TEXT NOT NULL,
		created_at DATETIME NOT NULL,
		restored_at DATETIME,
		restored_by TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_activity_event_corrections_activity
		ON activity_event_corrections(activity_id, created_at);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_activity_event_corrections_active_event
		ON activity_event_corrections(event_id) WHERE restored_at IS NULL;

	CREATE TABLE IF NOT EXISTS segments (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		camera TEXT NOT NULL,
		path TEXT NOT NULL UNIQUE,
		start_time DATETIME NOT NULL,
		end_time DATETIME NOT NULL,
		size_bytes INTEGER DEFAULT 0
	);

	CREATE INDEX IF NOT EXISTS idx_segments_camera_time ON segments(camera, start_time);
	CREATE INDEX IF NOT EXISTS idx_segments_end_time ON segments(end_time);

	CREATE TABLE IF NOT EXISTS zones (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		camera TEXT NOT NULL,
		name TEXT NOT NULL,
		points TEXT NOT NULL DEFAULT '[]',
		x1 REAL NOT NULL,
		y1 REAL NOT NULL,
		x2 REAL NOT NULL,
		y2 REAL NOT NULL,
		labels TEXT NOT NULL DEFAULT '[]',
		track_presence BOOLEAN NOT NULL DEFAULT 0,
		face_recognition BOOLEAN NOT NULL DEFAULT 0,
		enabled BOOLEAN NOT NULL DEFAULT 1,
		UNIQUE(camera, name)
	);

	CREATE TABLE IF NOT EXISTS zone_presence (
		zone_id INTEGER NOT NULL REFERENCES zones(id) ON DELETE CASCADE,
		label TEXT NOT NULL,
		present BOOLEAN NOT NULL DEFAULT 0,
		last_seen DATETIME,
		last_changed DATETIME,
		PRIMARY KEY (zone_id, label)
	);

	CREATE TABLE IF NOT EXISTS people (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT,
		ignore BOOLEAN NOT NULL DEFAULT 0,
		centroid BLOB,
		source_event_id TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS faces (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		event_id TEXT REFERENCES events(id) ON DELETE SET NULL,
		camera TEXT NOT NULL,
		person_id INTEGER REFERENCES people(id) ON DELETE SET NULL,
		embedding BLOB NOT NULL,
		crop_path TEXT,
		confidence REAL NOT NULL,
		similarity REAL,
		timestamp DATETIME NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_faces_person ON faces(person_id);
	CREATE INDEX IF NOT EXISTS idx_faces_timestamp ON faces(timestamp);

	CREATE TABLE IF NOT EXISTS auth_sessions (
		id TEXT PRIMARY KEY,
		username TEXT NOT NULL,
		csrf_token TEXT NOT NULL,
		remote_ip TEXT,
		user_agent TEXT,
		created_at DATETIME NOT NULL,
		last_seen_at DATETIME NOT NULL,
		expires_at DATETIME NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_auth_sessions_expires_at ON auth_sessions(expires_at);

	CREATE TABLE IF NOT EXISTS api_tokens (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT NOT NULL,
		name TEXT NOT NULL,
		token_prefix TEXT NOT NULL,
		token_hash BLOB NOT NULL UNIQUE,
		scopes TEXT NOT NULL DEFAULT '[]',
		created_at DATETIME NOT NULL,
		last_used_at DATETIME,
		revoked_at DATETIME
	);
	CREATE INDEX IF NOT EXISTS idx_api_tokens_username ON api_tokens(username);

	CREATE TABLE IF NOT EXISTS known_objects (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		label TEXT NOT NULL,
		centroid BLOB,
		crop_path TEXT,
		match_threshold REAL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS object_sightings (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		event_id TEXT REFERENCES events(id) ON DELETE SET NULL,
		camera TEXT NOT NULL,
		object_id INTEGER NOT NULL REFERENCES known_objects(id) ON DELETE CASCADE,
		similarity REAL NOT NULL,
		timestamp DATETIME NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_object_sightings_object ON object_sightings(object_id);
	CREATE INDEX IF NOT EXISTS idx_object_sightings_event ON object_sightings(event_id);

	CREATE TABLE IF NOT EXISTS object_references (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		object_id INTEGER NOT NULL REFERENCES known_objects(id) ON DELETE CASCADE,
		event_id TEXT,
		embedding BLOB NOT NULL,
		crop_path TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_object_references_object ON object_references(object_id);

	CREATE TABLE IF NOT EXISTS auth_users (
		username TEXT PRIMARY KEY,
		password_hash TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS motion_activity (
		camera TEXT NOT NULL,
		bucket DATETIME NOT NULL,
		score  REAL NOT NULL,
		PRIMARY KEY (camera, bucket)
	);

	CREATE TABLE IF NOT EXISTS kv_store (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS push_subscriptions (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		username    TEXT    NOT NULL,
		endpoint    TEXT    NOT NULL UNIQUE,
		p256dh      TEXT    NOT NULL,
		auth        TEXT    NOT NULL,
		user_agent  TEXT,
		created_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
		last_seen   DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_push_subs_user ON push_subscriptions(username);

	CREATE TABLE IF NOT EXISTS notification_prefs (
		username     TEXT NOT NULL,
		camera       TEXT NOT NULL,
		object_class TEXT NOT NULL,
		enabled      BOOLEAN NOT NULL DEFAULT 1,
		PRIMARY KEY (username, camera, object_class)
	);

	CREATE TABLE IF NOT EXISTS storage_audit (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		ts          TIMESTAMP NOT NULL,
		actor       TEXT NOT NULL,
		scope_json  TEXT NOT NULL,
		bytes_freed INTEGER NOT NULL,
		file_count  INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_storage_audit_ts ON storage_audit(ts DESC);
`

// legacyColumn is a column added after a table's original definition. Databases
// created before the column was introduced are backfilled by ensureColumn.
type legacyColumn struct {
	table  string
	column string
	ddl    string
}

// legacyColumns backfills databases created before column versioning (v0). On a
// fresh database these are all no-ops because baselineSchema already defines
// the columns. Each runs through ensureColumn, which surfaces real errors.
var legacyColumns = []legacyColumn{
	{"events", "end_time", "ALTER TABLE events ADD COLUMN end_time DATETIME"},
	{"events", "zone_name", "ALTER TABLE events ADD COLUMN zone_name TEXT"},
	{"events", "snapshot_available", "ALTER TABLE events ADD COLUMN snapshot_available BOOLEAN NOT NULL DEFAULT 0"},
	{"events", "clip_available", "ALTER TABLE events ADD COLUMN clip_available BOOLEAN NOT NULL DEFAULT 0"},
	{"events", "object_name", "ALTER TABLE events ADD COLUMN object_name TEXT"},
	{"events", "sub_label", "ALTER TABLE events ADD COLUMN sub_label TEXT"},
	{"zones", "points", "ALTER TABLE zones ADD COLUMN points TEXT NOT NULL DEFAULT '[]'"},
	{"people", "source_event_id", "ALTER TABLE people ADD COLUMN source_event_id TEXT"},
	{"known_objects", "match_threshold", "ALTER TABLE known_objects ADD COLUMN match_threshold REAL"},
	{"auth_sessions", "idle_ttl_seconds", "ALTER TABLE auth_sessions ADD COLUMN idle_ttl_seconds INTEGER NOT NULL DEFAULT 1800"},
	{"segments", "recompressed", "ALTER TABLE segments ADD COLUMN recompressed BOOLEAN NOT NULL DEFAULT FALSE"},
	{"segments", "recompressed_at", "ALTER TABLE segments ADD COLUMN recompressed_at DATETIME"},
	{"segments", "recompress_failures", "ALTER TABLE segments ADD COLUMN recompress_failures INT NOT NULL DEFAULT 0"},
}

// sqlExecutor is the subset of *sql.DB and *sql.Tx that the migration helpers
// use. Taking the interface lets a helper run inside a step's transaction while
// tests and non-migration callers keep passing a *sql.DB.
type sqlExecutor interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// migrationStep is one numbered upgrade of the schema. run executes inside a
// transaction that also stamps step.version, so the stored version and the work
// it claims happened commit together or not at all. skipped counts rows the step
// could not interpret and deliberately left as they are; any error abandons the
// entire step, version stamp included, so the next start retries it instead of
// running on a half-migrated database that reports itself as current.
type migrationStep struct {
	version int
	name    string
	run     func(tx *sql.Tx) (skipped int, err error)
}

// migrationSteps runs in order and covers every version from 1 through
// currentSchemaVersion, which TestMigrationStepsCoverEverySchemaVersion
// enforces. A database reports the version of the last step that committed.
var migrationSteps = []migrationStep{
	{
		version: 1,
		// Pre-versioning upgrade path. Databases at version 0 may be missing
		// later-added columns and may hold legacy (non-UTC) timestamps. Both
		// parts are idempotent, so a fresh DB runs them harmlessly.
		name: "backfill pre-versioning columns and normalize timestamps",
		run: func(tx *sql.Tx) (int, error) {
			for _, c := range legacyColumns {
				if err := ensureColumn(tx, c.table, c.column, c.ddl); err != nil {
					return 0, fmt.Errorf("backfill column %s.%s: %w", c.table, c.column, err)
				}
			}
			return normalizeTimestamps(tx)
		},
	},
	{
		version: 2,
		// Re-canonicalize every timestamp column into the driver's native Go
		// String() form. Version 1's normalize step only matched non-UTC or
		// monotonic timestamps, so RFC3339 ("T"-separated) rows slipped through.
		// Bare (index-using) comparisons require one uniform on-disk format.
		name: "recanonicalize timestamps",
		run:  func(tx *sql.Tx) (int, error) { return recanonicalizeTimestamps(tx) },
	},
	{
		version: 3,
		// Extends the events table with the same recompression-state pattern
		// used for segments. ensureColumn is idempotent, so an existing v2
		// database gains the columns and a fresh database (already covered by
		// baselineSchema) is a clean no-op. The index on (camera, end_time) is
		// created here rather than in baselineSchema because end_time is absent
		// from pre-v1 databases at baseline-schema time and would cause a
		// "no such column" error before the v1 backfill runs.
		name: "event clip recompression state",
		run: func(tx *sql.Tx) (int, error) {
			for _, c := range []legacyColumn{
				{"events", "recompressed", "ALTER TABLE events ADD COLUMN recompressed INTEGER NOT NULL DEFAULT 0"},
				{"events", "recompressed_at", "ALTER TABLE events ADD COLUMN recompressed_at DATETIME"},
				{"events", "recompress_failures", "ALTER TABLE events ADD COLUMN recompress_failures INTEGER NOT NULL DEFAULT 0"},
				{"events", "clip_size_bytes", "ALTER TABLE events ADD COLUMN clip_size_bytes INTEGER NOT NULL DEFAULT 0"},
			} {
				if err := ensureColumn(tx, c.table, c.column, c.ddl); err != nil {
					return 0, fmt.Errorf("backfill column %s.%s: %w", c.table, c.column, err)
				}
			}
			if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_events_clip_recompress ON events(camera, end_time)`); err != nil {
				return 0, fmt.Errorf("create idx_events_clip_recompress: %w", err)
			}
			return 0, nil
		},
	},
	{
		version: 4,
		// Events carry a review/notification category ("alert" or "detection").
		// Existing rows default to "alert" so no historical event is
		// reclassified or hidden.
		name: "event category",
		run: func(tx *sql.Tx) (int, error) {
			if err := ensureColumn(tx, "events", "category",
				"ALTER TABLE events ADD COLUMN category TEXT NOT NULL DEFAULT 'alert'"); err != nil {
				return 0, fmt.Errorf("backfill column events.category: %w", err)
			}
			return 0, nil
		},
	},
	{
		version: 5,
		name:    "doorbell event kind and answer state",
		run: func(tx *sql.Tx) (int, error) {
			for _, c := range []legacyColumn{
				{"events", "kind", "ALTER TABLE events ADD COLUMN kind TEXT NOT NULL DEFAULT 'object'"},
				{"events", "answered_at", "ALTER TABLE events ADD COLUMN answered_at DATETIME"},
				{"events", "answered_by", "ALTER TABLE events ADD COLUMN answered_by TEXT"},
			} {
				if err := ensureColumn(tx, c.table, c.column, c.ddl); err != nil {
					return 0, fmt.Errorf("backfill column %s.%s: %w", c.table, c.column, err)
				}
			}
			if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_events_kind ON events(kind)`); err != nil {
				return 0, fmt.Errorf("create idx_events_kind: %w", err)
			}
			return 0, nil
		},
	},
	{
		version: 6,
		// Early object-recognition builds could persist a sighting without its
		// event ID. Restore the relationship only when one retained event is an
		// exact, identity-confirmed match. Ambiguous rows and sightings whose
		// source event expired under retention deliberately remain unlinked.
		name: "restore object sighting event links",
		run:  restoreObjectSightingLinks,
	},
	{
		version: 7,
		// Activities group nearby evidence from one camera into the
		// incident-sized unit shown in review. Backfill walks events in
		// chronological order through the same deterministic aggregator used for
		// live writes.
		name: "backfill activities",
		run: func(tx *sql.Tx) (int, error) {
			if err := backfillActivities(tx); err != nil {
				return 0, fmt.Errorf("backfill activities: %w", err)
			}
			return 0, nil
		},
	},
	{
		version: 8,
		// Activities gain an explicit lifecycle. Historical activities are
		// finalized and marked as already considered for notifications so an
		// upgrade never floods users with old incidents. New live writes
		// explicitly create or reopen an activity in the open state.
		name: "activity lifecycle",
		run: func(tx *sql.Tx) (int, error) {
			for _, c := range []legacyColumn{
				{"activities", "state", "ALTER TABLE activities ADD COLUMN state TEXT NOT NULL DEFAULT 'finalized'"},
				{"activities", "finalized_at", "ALTER TABLE activities ADD COLUMN finalized_at DATETIME"},
				{"activities", "notification_queued_at", "ALTER TABLE activities ADD COLUMN notification_queued_at DATETIME"},
			} {
				if err := ensureColumn(tx, c.table, c.column, c.ddl); err != nil {
					return 0, fmt.Errorf("backfill column %s.%s: %w", c.table, c.column, err)
				}
			}
			if _, err := tx.Exec(`
				UPDATE activities
				SET state = 'finalized',
				    finalized_at = COALESCE(finalized_at, updated_at, end_time),
				    notification_queued_at = COALESCE(notification_queued_at, finalized_at, updated_at, end_time)`); err != nil {
				return 0, fmt.Errorf("finalize historical activities: %w", err)
			}
			if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_activities_state_end ON activities(state, end_time)`); err != nil {
				return 0, fmt.Errorf("create idx_activities_state_end: %w", err)
			}
			return 0, nil
		},
	},
	{
		version: 9,
		// Operator evidence corrections are durable, attributable, and
		// reversible. The baseline schema creates the new table for both fresh
		// and existing databases; this step states the version boundary
		// explicitly.
		name: "activity evidence corrections",
		run: func(tx *sql.Tx) (int, error) {
			if _, err := tx.Exec(`
				CREATE TABLE IF NOT EXISTS activity_event_corrections (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					activity_id TEXT NOT NULL REFERENCES activities(id) ON DELETE CASCADE,
					event_id TEXT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
					action TEXT NOT NULL CHECK(action = 'excluded'),
					reason TEXT NOT NULL DEFAULT '',
					actor TEXT NOT NULL,
					created_at DATETIME NOT NULL,
					restored_at DATETIME,
					restored_by TEXT
				);
				CREATE INDEX IF NOT EXISTS idx_activity_event_corrections_activity
					ON activity_event_corrections(activity_id, created_at);
				CREATE UNIQUE INDEX IF NOT EXISTS idx_activity_event_corrections_active_event
					ON activity_event_corrections(event_id) WHERE restored_at IS NULL;
			`); err != nil {
				return 0, fmt.Errorf("create activity evidence corrections: %w", err)
			}
			return 0, nil
		},
	},
}

func migrate(db *sql.DB) error {
	version, err := userVersion(db)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	// Baseline tables/indexes. Idempotent; needed to create a fresh database
	// and to add any wholly new tables to an existing one. It carries no version
	// stamp of its own, so re-running it after a failed step is harmless.
	if _, err := db.Exec(baselineSchema); err != nil {
		return fmt.Errorf("create baseline schema: %w", err)
	}

	totalSkipped := 0
	for _, step := range migrationSteps {
		if version >= step.version {
			continue
		}
		skipped, err := runMigrationStep(db, step)
		if err != nil {
			return fmt.Errorf("schema version %d (%s): %w", step.version, step.name, err)
		}
		totalSkipped += skipped
	}

	// Skipped rows are reported once with a total rather than one line per row:
	// a database with a systematic format problem has thousands of them, and the
	// per-row detail is available by raising the log level.
	if totalSkipped > 0 {
		slog.Warn("schema migration left rows it could not interpret unchanged",
			"skipped", totalSkipped, "schema_version", currentSchemaVersion)
	}
	return nil
}

// runMigrationStep applies one step and its version stamp in a single
// transaction.
func runMigrationStep(db *sql.DB, step migrationStep) (int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Stamping the version is the transaction's first statement so the
	// transaction takes SQLite's write lock before it reads anything. A
	// transaction that reads first and writes later cannot be upgraded once
	// another connection has committed in between: SQLite fails it with
	// SQLITE_BUSY_SNAPSHOT without consulting the busy handler, so an
	// overlapping migrator (the old process during a restart) would abort
	// instead of waiting its turn.
	if err := setUserVersion(tx, step.version); err != nil {
		return 0, fmt.Errorf("stamp schema version: %w", err)
	}

	skipped, err := step.run(tx)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return skipped, nil
}

// restoreObjectSightingLinks is the version 6 step.
func restoreObjectSightingLinks(tx *sql.Tx) (int, error) {
	result, err := tx.Exec(`
		UPDATE object_sightings AS s
		SET event_id = (
			SELECT e.id
			FROM events e
			JOIN known_objects o ON o.id = s.object_id
			WHERE e.camera = s.camera
				AND e.timestamp = s.timestamp
				AND e.label = o.label
				AND (e.object_name = o.name OR e.sub_label = o.name)
			LIMIT 1
		)
		WHERE (s.event_id IS NULL OR TRIM(s.event_id) = '')
			AND 1 = (
				SELECT COUNT(*)
				FROM events e
				JOIN known_objects o ON o.id = s.object_id
				WHERE e.camera = s.camera
					AND e.timestamp = s.timestamp
					AND e.label = o.label
					AND (e.object_name = o.name OR e.sub_label = o.name)
			)
	`)
	if err != nil {
		return 0, fmt.Errorf("restore object sighting event links: %w", err)
	}
	if restored, rowsErr := result.RowsAffected(); rowsErr == nil && restored > 0 {
		slog.Info("restored historical object sighting event links", "count", restored)
	}
	return 0, nil
}

func userVersion(db sqlExecutor) (int, error) {
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}

func setUserVersion(db sqlExecutor, v int) error {
	// PRAGMA user_version does not accept bound parameters; v is an internal
	// integer constant, not user input. The write participates in the
	// surrounding transaction, so a rolled-back step leaves the stored version
	// where it was.
	_, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", v))
	return err
}

// columnExists reports whether table has a column with the given name.
func columnExists(db sqlExecutor, table, column string) (bool, error) {
	// PRAGMA table_info does not accept bound parameters. table is an internal
	// constant from legacyColumns, never user input.
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%q)", table))
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			cid       int
			name      string
			colType   string
			notNull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// ensureColumn adds a column via ddl only when it is missing. Unlike the old
// fire-and-forget ALTER, it surfaces any error other than the column already
// existing, and is a clean no-op when the column is present.
func ensureColumn(db sqlExecutor, table, column, ddl string) error {
	exists, err := columnExists(db, table, column)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err := db.Exec(ddl); err != nil {
		// A concurrent migrator (e.g. a second process during a restart) may
		// have added the column between our check and our ALTER, in which case
		// SQLite returns a duplicate-column error. Re-check: a column that now
		// exists means the race was benign; a still-missing column is a real
		// failure that must surface.
		if present, checkErr := columnExists(db, table, column); checkErr == nil && present {
			return nil
		}
		return err
	}
	return nil
}

// normalizeTimestamps rewrites legacy timestamps stored in Go's String() format
// (with timezone/monotonic suffixes) to UTC, so SQLite text comparisons are
// consistent.
//
// A value the driver cannot read back as a time is counted as a skip and left
// as it is: the row is a data problem that re-running the migration would meet
// again, and inventing a timestamp for it would be worse than leaving it
// visibly unconverted. A failing write is the opposite case, so it aborts the
// step and with it the version stamp, and the next start tries again.
func normalizeTimestamps(db sqlExecutor) (int, error) {
	// Check if normalization is needed by looking at a sample segment.
	var sample string
	err := db.QueryRow("SELECT start_time FROM segments LIMIT 1").Scan(&sample)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, nil // empty DB; nothing to normalize
		}
		return 0, err
	}
	if !needsNormalization(sample) {
		return 0, nil
	}

	skipped := 0

	// Normalize segments.
	rows, err := db.Query("SELECT id, start_time, end_time FROM segments")
	if err != nil {
		return skipped, err
	}
	type segTime struct {
		id    int64
		start time.Time
		end   time.Time
	}
	var segs []segTime
	for rows.Next() {
		var s segTime
		if err := rows.Scan(&s.id, &s.start, &s.end); err != nil {
			slog.Debug("normalize segment timestamp: unreadable value skipped", "id", s.id, "error", err)
			skipped++
			continue
		}
		segs = append(segs, s)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return skipped, fmt.Errorf("read segment timestamps: %w", err)
	}
	_ = rows.Close()

	for _, s := range segs {
		if _, err := db.Exec("UPDATE segments SET start_time = ?, end_time = ? WHERE id = ?",
			s.start.UTC().Round(0), s.end.UTC().Round(0), s.id); err != nil {
			return skipped, fmt.Errorf("normalize segment %d: %w", s.id, err)
		}
	}

	// Normalize events.
	erows, err := db.Query("SELECT id, timestamp, end_time FROM events")
	if err != nil {
		return skipped, err
	}
	type evtTime struct {
		id      string
		ts      time.Time
		endTime sql.NullTime
	}
	var evts []evtTime
	for erows.Next() {
		var e evtTime
		if err := erows.Scan(&e.id, &e.ts, &e.endTime); err != nil {
			slog.Debug("normalize event timestamp: unreadable value skipped", "id", e.id, "error", err)
			skipped++
			continue
		}
		evts = append(evts, e)
	}
	if err := erows.Err(); err != nil {
		_ = erows.Close()
		return skipped, fmt.Errorf("read event timestamps: %w", err)
	}
	_ = erows.Close()

	for _, e := range evts {
		var execErr error
		if e.endTime.Valid {
			_, execErr = db.Exec("UPDATE events SET timestamp = ?, end_time = ? WHERE id = ?",
				e.ts.UTC().Round(0), e.endTime.Time.UTC().Round(0), e.id)
		} else {
			_, execErr = db.Exec("UPDATE events SET timestamp = ? WHERE id = ?",
				e.ts.UTC().Round(0), e.id)
		}
		if execErr != nil {
			return skipped, fmt.Errorf("normalize event %s: %w", e.id, execErr)
		}
	}
	return skipped, nil
}

// needsNormalization returns true if a stored timestamp string contains
// non-UTC timezone info or monotonic clock readings.
func needsNormalization(s string) bool {
	return strings.Contains(s, "m=+") || strings.Contains(s, "m=-") ||
		(strings.Contains(s, "+") && !strings.HasSuffix(strings.TrimSpace(s), "+0000 UTC"))
}

// timestampColumns lists every DATETIME column that the application writes via
// utc() AND that is compared or ordered as a string (range query, WHERE, or
// ORDER BY). recanonicalizeTimestamps rewrites them into one uniform format so
// bare (index-using) comparisons are correct.
//
// Columns whose values come from SQLite's CURRENT_TIMESTAMP default rather than
// utc() (e.g. people.created_at, auth_users.created_at, whose INSERTs omit the
// column) are deliberately excluded: SQLite writes those as "2006-01-02
// 15:04:05" with no zone suffix, so recanonicalizing existing rows to the
// " +0000 UTC" form would make them sort in a different lexicographic group than
// every future CURRENT_TIMESTAMP insert. A column that has a CURRENT_TIMESTAMP
// default but is always overwritten by an explicit utc() value (e.g.
// object_references.created_at) is included - its on-disk form is canonical.
var timestampColumns = []struct {
	table  string
	column string
}{
	{"segments", "start_time"},
	{"segments", "end_time"},
	{"events", "timestamp"},
	{"events", "end_time"},
	{"motion_activity", "bucket"},
	{"faces", "timestamp"},
	{"object_sightings", "timestamp"},
	{"auth_sessions", "expires_at"},
	{"api_tokens", "created_at"},
	{"object_references", "created_at"},
	{"storage_audit", "ts"},
}

// storedTimeLayouts are the historical on-disk timestamp formats, tried in turn
// when parsing a stored value back into a time.Time.
var storedTimeLayouts = []string{
	"2006-01-02 15:04:05.999999999 -0700 MST",
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02T15:04:05.999999999Z",
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05",
}

// parseStoredTime parses a timestamp stored by any historical code path.
func parseStoredTime(s string) (time.Time, error) {
	for _, layout := range storedTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable timestamp: %s", s)
}

// isCanonicalTimestamp reports whether s is already in the driver's native UTC
// form: "2006-01-02 15:04:05[.fraction] +0000 UTC". The date and time are
// separated by a space at index 10 (an RFC3339 value has a "T" there); the
// "UTC" suffix legitimately contains a "T", so the separator position - not a
// substring search - is what distinguishes the two. Canonical rows are skipped
// to avoid needless writes on an already-normalized database.
func isCanonicalTimestamp(s string) bool {
	return len(s) > 10 && s[10] == ' ' && strings.HasSuffix(s, " +0000 UTC")
}

// recanonicalizeTimestamps rewrites any non-canonical timestamp into the
// driver's native format by round-tripping it through time.Time. Rows already
// canonical are left untouched. A value no known layout parses is counted and
// skipped so one bad row cannot block startup forever, and the count is
// reported once at the end of the migration.
func recanonicalizeTimestamps(db sqlExecutor) (int, error) {
	total := 0
	for _, tc := range timestampColumns {
		skipped, err := recanonicalizeColumn(db, tc.table, tc.column)
		total += skipped
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func recanonicalizeColumn(db sqlExecutor, table, column string) (int, error) {
	// table/column are internal constants from timestampColumns, never user
	// input, so string interpolation is safe here. CAST(... AS TEXT) returns the
	// true on-disk bytes; scanning a DATETIME column into a string instead makes
	// the driver reformat the value, masking the stored representation.
	query := fmt.Sprintf("SELECT rowid, CAST(%s AS TEXT) FROM %s WHERE %s IS NOT NULL", column, table, column)
	rows, err := db.Query(query)
	if err != nil {
		return 0, err
	}
	type row struct {
		rowid int64
		raw   string
	}
	var stale []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.rowid, &r.raw); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if !isCanonicalTimestamp(r.raw) {
			stale = append(stale, r)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	_ = rows.Close()

	skipped := 0
	update := fmt.Sprintf("UPDATE %s SET %s = ? WHERE rowid = ?", table, column)
	for _, r := range stale {
		t, perr := parseStoredTime(r.raw)
		if perr != nil {
			slog.Debug("recanonicalize timestamp: unparseable value skipped", "table", table, "column", column, "rowid", r.rowid, "value", r.raw)
			skipped++
			continue
		}
		// A failing write means the rows this step already rewrote and the ones
		// it has not are inconsistent, so it abandons the step rather than
		// leaving the database stamped as migrated.
		if _, err := db.Exec(update, utc(t), r.rowid); err != nil {
			return skipped, fmt.Errorf("recanonicalize %s.%s rowid %d: %w", table, column, r.rowid, err)
		}
	}
	return skipped, nil
}
