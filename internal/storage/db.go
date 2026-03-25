package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/rvben/vedetta/internal/camera"
	_ "modernc.org/sqlite"
)

// needsNormalization returns true if a stored timestamp string contains
// non-UTC timezone info or monotonic clock readings.
func needsNormalization(s string) bool {
	return strings.Contains(s, "m=+") || strings.Contains(s, "m=-") ||
		(strings.Contains(s, "+") && !strings.HasSuffix(strings.TrimSpace(s), "+0000 UTC"))
}

// SegmentRecord represents a recorded video segment stored in the database.
type SegmentRecord struct {
	ID        int64
	Camera    string
	Path      string
	StartTime time.Time
	EndTime   time.Time
	SizeBytes int64
}

// MotionBucket represents a single minute-level motion activity score for a camera.
type MotionBucket struct {
	Bucket time.Time
	Score  float64
}

// DB wraps SQLite for event storage.
type DB struct {
	db *sql.DB
}

func New(path string) (*DB, error) {
	// PRAGMAs in the DSN are applied to every new connection in the pool.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// WAL mode permits one writer + multiple readers simultaneously.
	// busy_timeout (5s in DSN) retries when the write lock is held.
	// Default pool size is unlimited; the Go sql.DB pool handles reuse.
	// We only set idle connection limits to avoid resource waste.
	db.SetMaxIdleConns(4)

	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return &DB{db: db}, nil
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
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
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);

		CREATE INDEX IF NOT EXISTS idx_events_camera ON events(camera);
		CREATE INDEX IF NOT EXISTS idx_events_timestamp ON events(timestamp);
		CREATE INDEX IF NOT EXISTS idx_events_label ON events(label);

		CREATE TABLE IF NOT EXISTS segments (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			camera TEXT NOT NULL,
			path TEXT NOT NULL UNIQUE,
			start_time DATETIME NOT NULL,
			end_time DATETIME NOT NULL,
			size_bytes INTEGER DEFAULT 0
		);

		CREATE INDEX IF NOT EXISTS idx_segments_camera_time ON segments(camera, start_time);

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
	`)
	if err != nil {
		return err
	}

	// Add end_time column to existing databases
	_, _ = db.Exec("ALTER TABLE events ADD COLUMN end_time DATETIME")

	// Add zone_name column to existing databases
	_, _ = db.Exec("ALTER TABLE events ADD COLUMN zone_name TEXT")
	_, _ = db.Exec("ALTER TABLE events ADD COLUMN snapshot_available BOOLEAN NOT NULL DEFAULT 0")
	_, _ = db.Exec("ALTER TABLE events ADD COLUMN clip_available BOOLEAN NOT NULL DEFAULT 0")
	_, _ = db.Exec("ALTER TABLE zones ADD COLUMN points TEXT NOT NULL DEFAULT '[]'")
	_, _ = db.Exec("ALTER TABLE events ADD COLUMN object_name TEXT")
	_, _ = db.Exec("ALTER TABLE people ADD COLUMN source_event_id TEXT")
	_, _ = db.Exec("ALTER TABLE known_objects ADD COLUMN match_threshold REAL")
	_, _ = db.Exec("ALTER TABLE events ADD COLUMN sub_label TEXT")
	_, _ = db.Exec("ALTER TABLE auth_sessions ADD COLUMN idle_ttl_seconds INTEGER NOT NULL DEFAULT 1800")

	// Normalize timestamps to UTC RFC3339 format for consistent SQLite comparisons.
	// The modernc.org/sqlite driver stores time.Time using Go's String() which includes
	// timezone and monotonic clock, breaking text-based comparisons across timezones.
	normalizeTimestamps(db)

	return nil
}

func normalizeTimestamps(db *sql.DB) {
	// Check if normalization is needed by looking at a sample segment
	var sample string
	err := db.QueryRow("SELECT start_time FROM segments LIMIT 1").Scan(&sample)
	if err != nil || !needsNormalization(sample) {
		return
	}

	// Normalize segments
	rows, err := db.Query("SELECT id, start_time, end_time FROM segments")
	if err != nil {
		return
	}
	type segTime struct {
		id    int64
		start time.Time
		end   time.Time
	}
	var segs []segTime
	for rows.Next() {
		var s segTime
		if err := rows.Scan(&s.id, &s.start, &s.end); err == nil {
			segs = append(segs, s)
		}
	}
	rows.Close()

	for _, s := range segs {
		db.Exec("UPDATE segments SET start_time = ?, end_time = ? WHERE id = ?",
			s.start.UTC().Round(0), s.end.UTC().Round(0), s.id)
	}

	// Normalize events
	erows, err := db.Query("SELECT id, timestamp, end_time FROM events")
	if err != nil {
		return
	}
	type evtTime struct {
		id      string
		ts      time.Time
		endTime sql.NullTime
	}
	var evts []evtTime
	for erows.Next() {
		var e evtTime
		if err := erows.Scan(&e.id, &e.ts, &e.endTime); err == nil {
			evts = append(evts, e)
		}
	}
	erows.Close()

	for _, e := range evts {
		if e.endTime.Valid {
			db.Exec("UPDATE events SET timestamp = ?, end_time = ? WHERE id = ?",
				e.ts.UTC().Round(0), e.endTime.Time.UTC().Round(0), e.id)
		} else {
			db.Exec("UPDATE events SET timestamp = ? WHERE id = ?",
				e.ts.UTC().Round(0), e.id)
		}
	}
}

func (d *DB) Close() error {
	return d.db.Close()
}

// Ping checks database connectivity by executing a simple query.
func (d *DB) Ping() error {
	var n int
	return d.db.QueryRow("SELECT 1").Scan(&n)
}

func (d *DB) SaveEvent(event camera.Event) error {
	var endTime *time.Time
	if !event.EndTime.IsZero() {
		t := utc(event.EndTime)
		endTime = &t
	}
	var zoneName *string
	if event.ZoneName != "" {
		zoneName = &event.ZoneName
	}
	_, err := d.db.Exec(`
		INSERT INTO events (id, camera, label, score, box_x1, box_y1, box_x2, box_y2, timestamp, end_time, snapshot_path, snapshot_available, clip_path, clip_available, zone_name, object_name, sub_label)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID, event.CameraName, event.Label, event.Score,
		event.Box[0], event.Box[1], event.Box[2], event.Box[3],
		utc(event.Timestamp), endTime, event.SnapshotPath, event.SnapshotAvailable, event.ClipPath, event.ClipAvailable, zoneName, nullString(event.ObjectName), nullString(event.SubLabel),
	)
	return err
}

func (d *DB) UpdateEventEndTime(eventID string, endTime time.Time) error {
	_, err := d.db.Exec("UPDATE events SET end_time = ? WHERE id = ?", utc(endTime), eventID)
	return err
}

func (d *DB) UpdateEventClipPath(eventID, clipPath string) error {
	_, err := d.db.Exec("UPDATE events SET clip_path = ?, clip_available = ? WHERE id = ?", clipPath, clipPath != "", eventID)
	return err
}

func (d *DB) UpdateEventSnapshotPath(eventID, snapshotPath string) error {
	_, err := d.db.Exec("UPDATE events SET snapshot_path = ?, snapshot_available = ? WHERE id = ?", snapshotPath, snapshotPath != "", eventID)
	return err
}

func (d *DB) UpdateEventSnapshotAvailability(eventID string, available bool) error {
	_, err := d.db.Exec("UPDATE events SET snapshot_available = ? WHERE id = ?", available, eventID)
	return err
}

func (d *DB) UpdateEventClipAvailability(eventID string, available bool) error {
	_, err := d.db.Exec("UPDATE events SET clip_available = ? WHERE id = ?", available, eventID)
	return err
}

// QueryEvents returns events matching the given filters.
func (d *DB) QueryEvents(cameraName, label string, limit, offset int) ([]camera.Event, error) {
	return d.QueryEventsFiltered(cameraName, label, "", "", limit, offset)
}

// QueryEventsFiltered returns events matching all given filters including zone.
func (d *DB) QueryEventsFiltered(cameraName, label, zoneName, objectName string, limit, offset int) ([]camera.Event, error) {
	query := "SELECT id, camera, label, score, box_x1, box_y1, box_x2, box_y2, timestamp, end_time, snapshot_path, snapshot_available, clip_path, clip_available, zone_name, object_name, sub_label FROM events WHERE 1=1"
	args := []any{}

	if cameraName != "" {
		query += " AND camera = ?"
		args = append(args, cameraName)
	}
	if label != "" {
		query += " AND label = ?"
		args = append(args, label)
	}
	if zoneName != "" {
		query += " AND zone_name = ?"
		args = append(args, zoneName)
	}
	if objectName != "" {
		query += " AND (object_name = ? OR sub_label = ?)"
		args = append(args, objectName, objectName)
	}

	query += " ORDER BY timestamp DESC"

	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	if offset > 0 {
		query += " OFFSET ?"
		args = append(args, offset)
	}

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return scanEvents(rows)
}

// CountEventsByLabel returns the count of events grouped by label.
func (d *DB) CountEventsByLabel() (map[string]int, error) {
	rows, err := d.db.Query("SELECT label, COUNT(*) FROM events GROUP BY label")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	result := make(map[string]int)
	for rows.Next() {
		var label string
		var count int
		if err := rows.Scan(&label, &count); err != nil {
			return nil, err
		}
		result[label] = count
	}
	return result, rows.Err()
}

// CountEventsByCamera returns the count of events grouped by camera name.
func (d *DB) CountEventsByCamera() (map[string]int, error) {
	rows, err := d.db.Query("SELECT camera, COUNT(*) FROM events GROUP BY camera")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	result := make(map[string]int)
	for rows.Next() {
		var cam string
		var count int
		if err := rows.Scan(&cam, &count); err != nil {
			return nil, err
		}
		result[cam] = count
	}
	return result, rows.Err()
}

// CountEvents returns the total number of events.
func (d *DB) CountEvents() (int, error) {
	var count int
	err := d.db.QueryRow("SELECT COUNT(*) FROM events").Scan(&count)
	return count, err
}

// utc normalizes a time.Time to UTC and strips the monotonic clock reading.
// This ensures consistent text representation in SQLite for correct comparisons.
func utc(t time.Time) time.Time {
	return t.UTC().Round(0)
}

// SaveSegment inserts or replaces a segment record in the database.
func (d *DB) SaveSegment(seg SegmentRecord) error {
	_, err := d.db.Exec(`
		INSERT OR REPLACE INTO segments (camera, path, start_time, end_time, size_bytes)
		VALUES (?, ?, ?, ?, ?)`,
		seg.Camera, seg.Path, utc(seg.StartTime), utc(seg.EndTime), seg.SizeBytes,
	)
	return err
}

// SaveMotionActivity inserts or replaces a motion activity score for a camera and minute bucket.
func (d *DB) SaveMotionActivity(camera string, bucket time.Time, score float64) error {
	_, err := d.db.Exec("INSERT OR REPLACE INTO motion_activity (camera, bucket, score) VALUES (?, ?, ?)", camera, utc(bucket), score)
	return err
}

// GetMotionActivity returns all motion buckets for a camera on the same UTC day as the given date.
func (d *DB) GetMotionActivity(camera string, date time.Time) ([]MotionBucket, error) {
	dayStart := date.UTC().Truncate(24 * time.Hour)
	dayEnd := dayStart.Add(24 * time.Hour)
	rows, err := d.db.Query("SELECT bucket, score FROM motion_activity WHERE camera = ? AND bucket >= ? AND bucket < ? ORDER BY bucket", camera, dayStart, dayEnd)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var buckets []MotionBucket
	for rows.Next() {
		var b MotionBucket
		if err := rows.Scan(&b.Bucket, &b.Score); err != nil {
			return nil, err
		}
		buckets = append(buckets, b)
	}
	return buckets, rows.Err()
}

// DeleteMotionActivityBefore removes all motion activity records older than the cutoff.
func (d *DB) DeleteMotionActivityBefore(cutoff time.Time) error {
	_, err := d.db.Exec("DELETE FROM motion_activity WHERE bucket < ?", utc(cutoff))
	return err
}

// QuerySegments returns segments for a camera that overlap the given time range.
func (d *DB) QuerySegments(cameraName string, from, to time.Time) ([]SegmentRecord, error) {
	rows, err := d.db.Query(`
		SELECT id, camera, path, start_time, end_time, size_bytes
		FROM segments
		WHERE camera = ? AND start_time < ? AND end_time > ?
		ORDER BY start_time`,
		cameraName, utc(to), utc(from),
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return scanSegments(rows)
}

// DeleteSegment removes a segment record by path.
func (d *DB) DeleteSegment(path string) error {
	_, err := d.db.Exec("DELETE FROM segments WHERE path = ?", path)
	return err
}

// GetAllSegments returns all segment records for a given camera.
func (d *DB) GetAllSegments(cameraName string) ([]SegmentRecord, error) {
	rows, err := d.db.Query(`
		SELECT id, camera, path, start_time, end_time, size_bytes
		FROM segments
		WHERE camera = ?
		ORDER BY start_time`,
		cameraName,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return scanSegments(rows)
}

// GetSegmentByPath returns a single segment record by its file path, or nil if not found.
func (d *DB) GetSegmentByPath(path string) (*SegmentRecord, error) {
	row := d.db.QueryRow(`
		SELECT id, camera, path, start_time, end_time, size_bytes
		FROM segments WHERE path = ?`, path)

	var seg SegmentRecord
	err := row.Scan(&seg.ID, &seg.Camera, &seg.Path, &seg.StartTime, &seg.EndTime, &seg.SizeBytes)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &seg, nil
}

// CountEventsToday returns the number of events with timestamp >= today midnight UTC.
func (d *DB) CountEventsToday() (int, error) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	var count int
	err := d.db.QueryRow("SELECT COUNT(*) FROM events WHERE timestamp >= ?", today).Scan(&count)
	return count, err
}

// GetEventByID returns a single event by ID, or nil if not found.
func (d *DB) GetEventByID(id string) (*camera.Event, error) {
	row := d.db.QueryRow(`
		SELECT id, camera, label, score, box_x1, box_y1, box_x2, box_y2, timestamp, end_time, snapshot_path, snapshot_available, clip_path, clip_available, zone_name, object_name, sub_label
		FROM events WHERE id = ?`, id)

	var e camera.Event
	var endTime sql.NullTime
	var snapshot, clip, zoneName, objectName, subLabel sql.NullString
	var snapshotAvailable, clipAvailable bool
	err := row.Scan(&e.ID, &e.CameraName, &e.Label, &e.Score,
		&e.Box[0], &e.Box[1], &e.Box[2], &e.Box[3],
		&e.Timestamp, &endTime, &snapshot, &snapshotAvailable, &clip, &clipAvailable, &zoneName, &objectName, &subLabel,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if endTime.Valid {
		e.EndTime = endTime.Time
	}
	e.SnapshotPath = snapshot.String
	e.SnapshotAvailable = snapshotAvailable
	e.ClipPath = clip.String
	e.ClipAvailable = clipAvailable
	e.ObjectName = objectName.String
	e.SubLabel = subLabel.String
	e.ZoneName = zoneName.String
	return &e, nil
}

// TotalStorageBytes returns the sum of size_bytes across all segments.
func (d *DB) TotalStorageBytes() (int64, error) {
	var total sql.NullInt64
	err := d.db.QueryRow("SELECT SUM(size_bytes) FROM segments").Scan(&total)
	if err != nil {
		return 0, err
	}
	return total.Int64, nil
}

// GetSegmentsForDate returns segments for a camera where start_time falls on the given date.
// If cameraName is empty, returns segments for all cameras.
func (d *DB) GetSegmentsForDate(cameraName string, date time.Time) ([]SegmentRecord, error) {
	dayStart := date.UTC().Truncate(24 * time.Hour)
	dayEnd := dayStart.Add(24 * time.Hour)

	var rows *sql.Rows
	var err error
	if cameraName != "" {
		rows, err = d.db.Query(`
			SELECT id, camera, path, start_time, end_time, size_bytes
			FROM segments
			WHERE camera = ? AND start_time >= ? AND start_time < ?
			ORDER BY start_time`,
			cameraName, dayStart, dayEnd,
		)
	} else {
		rows, err = d.db.Query(`
			SELECT id, camera, path, start_time, end_time, size_bytes
			FROM segments
			WHERE start_time >= ? AND start_time < ?
			ORDER BY start_time`,
			dayStart, dayEnd,
		)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return scanSegments(rows)
}

// QueryEventsForDate returns events for a camera on a given date (UTC day).
func (d *DB) QueryEventsForDate(cameraName string, date time.Time) ([]camera.Event, error) {
	dayStart := date.UTC().Truncate(24 * time.Hour)
	dayEnd := dayStart.Add(24 * time.Hour)

	rows, err := d.db.Query(`
		SELECT id, camera, label, score, box_x1, box_y1, box_x2, box_y2, timestamp, end_time, snapshot_path, snapshot_available, clip_path, clip_available, zone_name, object_name, sub_label
		FROM events
		WHERE camera = ? AND timestamp >= ? AND timestamp < ?
		ORDER BY timestamp`,
		cameraName, dayStart, dayEnd,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return scanEvents(rows)
}

// CountSegments returns the total number of segments.
func (d *DB) CountSegments() (int, error) {
	var count int
	err := d.db.QueryRow("SELECT COUNT(*) FROM segments").Scan(&count)
	return count, err
}

// TotalSegmentBytes returns the total bytes across all segments.
func (d *DB) TotalSegmentBytes() (int64, error) {
	return d.TotalStorageBytes()
}

// SegmentBytesByCamera returns total bytes grouped by camera name.
func (d *DB) SegmentBytesByCamera() (map[string]int64, error) {
	rows, err := d.db.Query("SELECT camera, SUM(size_bytes) FROM segments GROUP BY camera")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	result := make(map[string]int64)
	for rows.Next() {
		var cam string
		var total sql.NullInt64
		if err := rows.Scan(&cam, &total); err != nil {
			return nil, err
		}
		result[cam] = total.Int64
	}
	return result, rows.Err()
}

// GetOldestSegments returns the N oldest segments across all cameras, ordered by start_time.
func (d *DB) GetOldestSegments(limit int) ([]SegmentRecord, error) {
	rows, err := d.db.Query(`
		SELECT id, camera, path, start_time, end_time, size_bytes
		FROM segments
		ORDER BY start_time ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return scanSegments(rows)
}

// GetRecordingDays returns sorted day numbers that have segments for the given camera and month.
// If camera is empty, returns days across all cameras.
func (d *DB) GetRecordingDays(camera string, year int, month int) ([]int, error) {
	monthStart := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
	monthEnd := monthStart.AddDate(0, 1, 0)

	var rows *sql.Rows
	var err error
	if camera != "" {
		rows, err = d.db.Query(`
			SELECT DISTINCT CAST(substr(start_time, 9, 2) AS INTEGER) AS day
			FROM segments
			WHERE camera = ? AND start_time >= ? AND start_time < ?
			ORDER BY day`, camera, monthStart, monthEnd)
	} else {
		rows, err = d.db.Query(`
			SELECT DISTINCT CAST(substr(start_time, 9, 2) AS INTEGER) AS day
			FROM segments
			WHERE start_time >= ? AND start_time < ?
			ORDER BY day`, monthStart, monthEnd)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var days []int
	for rows.Next() {
		var day int
		if err := rows.Scan(&day); err != nil {
			return nil, err
		}
		days = append(days, day)
	}
	return days, rows.Err()
}

// GetAdjacentEvents returns the previous and next event IDs relative to the given event,
// ordered by timestamp.
func (d *DB) GetAdjacentEvents(id string) (prevID, nextID string, err error) {
	err = d.db.QueryRow(`
		SELECT id FROM events
		WHERE timestamp < (SELECT timestamp FROM events WHERE id = ?)
		ORDER BY timestamp DESC LIMIT 1`, id).Scan(&prevID)
	if err == sql.ErrNoRows {
		prevID = ""
		err = nil
	}
	if err != nil {
		return "", "", err
	}

	err = d.db.QueryRow(`
		SELECT id FROM events
		WHERE timestamp > (SELECT timestamp FROM events WHERE id = ?)
		ORDER BY timestamp ASC LIMIT 1`, id).Scan(&nextID)
	if err == sql.ErrNoRows {
		nextID = ""
		err = nil
	}
	if err != nil {
		return "", "", err
	}

	return prevID, nextID, nil
}

func scanEvents(rows *sql.Rows) ([]camera.Event, error) {
	var events []camera.Event
	for rows.Next() {
		var e camera.Event
		var endTime sql.NullTime
		var snapshot, clip, zoneName, objectName, subLabel sql.NullString
		var snapshotAvailable, clipAvailable bool
		err := rows.Scan(&e.ID, &e.CameraName, &e.Label, &e.Score,
			&e.Box[0], &e.Box[1], &e.Box[2], &e.Box[3],
			&e.Timestamp, &endTime, &snapshot, &snapshotAvailable, &clip, &clipAvailable, &zoneName, &objectName, &subLabel,
		)
		if err != nil {
			return nil, err
		}
		if endTime.Valid {
			e.EndTime = endTime.Time
		}
		e.SnapshotPath = snapshot.String
		e.SnapshotAvailable = snapshotAvailable
		e.ClipPath = clip.String
		e.ClipAvailable = clipAvailable
		e.ZoneName = zoneName.String
		e.ObjectName = objectName.String
		e.SubLabel = subLabel.String
		events = append(events, e)
	}
	return events, rows.Err()
}

// DeleteEvent removes an event by ID.
func (d *DB) DeleteEvent(id string) error {
	_, err := d.db.Exec("DELETE FROM events WHERE id = ?", id)
	return err
}

// EventsWithSnapshots returns all events that have a non-empty snapshot_path.
func (d *DB) EventsWithSnapshots() ([]camera.Event, error) {
	rows, err := d.db.Query(`
		SELECT id, camera, label, score, box_x1, box_y1, box_x2, box_y2, timestamp, end_time, snapshot_path, snapshot_available, clip_path, clip_available, zone_name, object_name, sub_label
		FROM events WHERE (snapshot_path != '' AND snapshot_path IS NOT NULL) OR (clip_path != '' AND clip_path IS NOT NULL)`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanEvents(rows)
}

func (d *DB) DeleteEventsOlderThan(cutoff time.Time) error {
	_, err := d.db.Exec("DELETE FROM events WHERE timestamp < ?", utc(cutoff))
	return err
}

func (d *DB) DeleteFacesOlderThan(cutoff time.Time) error {
	_, err := d.db.Exec(`
		DELETE FROM faces
		WHERE timestamp < ?
		   OR event_id IN (SELECT id FROM events WHERE timestamp < ?)`,
		utc(cutoff), utc(cutoff),
	)
	return err
}

func scanSegments(rows *sql.Rows) ([]SegmentRecord, error) {
	var segments []SegmentRecord
	for rows.Next() {
		var seg SegmentRecord
		if err := rows.Scan(&seg.ID, &seg.Camera, &seg.Path, &seg.StartTime, &seg.EndTime, &seg.SizeBytes); err != nil {
			return nil, err
		}
		segments = append(segments, seg)
	}
	return segments, rows.Err()
}

// --- Zone operations ---

// ListZones returns all zones for a camera.
func (d *DB) ListZones(cameraName string) ([]camera.Zone, error) {
	rows, err := d.db.Query(`
		SELECT id, camera, name, points, x1, y1, x2, y2, labels, track_presence, face_recognition, enabled
		FROM zones WHERE camera = ? ORDER BY name`, cameraName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return scanZones(rows)
}

// GetZone returns a single zone by camera and name, or nil if not found.
func (d *DB) GetZone(cameraName, name string) (*camera.Zone, error) {
	row := d.db.QueryRow(`
		SELECT id, camera, name, points, x1, y1, x2, y2, labels, track_presence, face_recognition, enabled
		FROM zones WHERE camera = ? AND name = ?`, cameraName, name)

	z, err := scanZone(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return z, nil
}

// SaveZone upserts a zone by camera+name.
func (d *DB) SaveZone(z camera.Zone) error {
	if len(z.Points) == 0 {
		z.Points = [][]float64{
			{z.X1, z.Y1},
			{z.X2, z.Y1},
			{z.X2, z.Y2},
			{z.X1, z.Y2},
		}
	}
	labelsJSON, err := json.Marshal(z.Labels)
	if err != nil {
		return fmt.Errorf("marshal labels: %w", err)
	}
	pointsJSON, err := json.Marshal(z.Points)
	if err != nil {
		return fmt.Errorf("marshal points: %w", err)
	}
	x1, y1, x2, y2 := zoneBounds(z.Points)

	_, err = d.db.Exec(`
		INSERT INTO zones (camera, name, points, x1, y1, x2, y2, labels, track_presence, face_recognition, enabled)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(camera, name) DO UPDATE SET
			points = excluded.points,
			x1 = excluded.x1,
			y1 = excluded.y1,
			x2 = excluded.x2,
			y2 = excluded.y2,
			labels = excluded.labels,
			track_presence = excluded.track_presence,
			face_recognition = excluded.face_recognition,
			enabled = excluded.enabled`,
		z.Camera, z.Name, string(pointsJSON), x1, y1, x2, y2,
		string(labelsJSON), z.TrackPresence, z.FaceRecognition, z.Enabled,
	)
	return err
}

// DeleteZone removes a zone by camera and name.
func (d *DB) DeleteZone(cameraName, name string) error {
	_, err := d.db.Exec("DELETE FROM zones WHERE camera = ? AND name = ?", cameraName, name)
	return err
}

// GetZonePresence returns the presence state for all labels in a zone.
func (d *DB) GetZonePresence(zoneID int) ([]camera.ZonePresence, error) {
	rows, err := d.db.Query(`
		SELECT zone_id, label, present, last_seen, last_changed
		FROM zone_presence WHERE zone_id = ?`, zoneID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var result []camera.ZonePresence
	for rows.Next() {
		var zp camera.ZonePresence
		var lastSeen, lastChanged sql.NullTime
		if err := rows.Scan(&zp.ZoneID, &zp.Label, &zp.Present, &lastSeen, &lastChanged); err != nil {
			return nil, err
		}
		if lastSeen.Valid {
			zp.LastSeen = lastSeen.Time
		}
		if lastChanged.Valid {
			zp.LastChanged = lastChanged.Time
		}
		result = append(result, zp)
	}
	return result, rows.Err()
}

// UpdateZonePresence upserts a zone presence record.
func (d *DB) UpdateZonePresence(zoneID int, label string, present bool) error {
	now := utc(time.Now())
	_, err := d.db.Exec(`
		INSERT INTO zone_presence (zone_id, label, present, last_seen, last_changed)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(zone_id, label) DO UPDATE SET
			present = excluded.present,
			last_seen = excluded.last_seen,
			last_changed = CASE WHEN zone_presence.present != excluded.present THEN excluded.last_changed ELSE zone_presence.last_changed END`,
		zoneID, label, present, now, now,
	)
	return err
}

// LatestObjectNameForZone returns the object_name from the most recent event
// matching the given zone and label, or "" if none found.
func (d *DB) LatestObjectNameForZone(zoneName, label string) string {
	var name sql.NullString
	d.db.QueryRow(`SELECT object_name FROM events WHERE zone_name = ? AND label = ? AND object_name IS NOT NULL AND object_name != '' ORDER BY timestamp DESC LIMIT 1`, zoneName, label).Scan(&name)
	return name.String
}

// UpdateEventZone sets the zone_name on an event.
func (d *DB) UpdateEventZone(eventID, zoneName string) error {
	_, err := d.db.Exec("UPDATE events SET zone_name = ? WHERE id = ?", zoneName, eventID)
	return err
}

func scanZones(rows *sql.Rows) ([]camera.Zone, error) {
	var zones []camera.Zone
	for rows.Next() {
		var z camera.Zone
		var pointsJSON, labelsJSON string
		err := rows.Scan(&z.ID, &z.Camera, &z.Name, &pointsJSON, &z.X1, &z.Y1, &z.X2, &z.Y2,
			&labelsJSON, &z.TrackPresence, &z.FaceRecognition, &z.Enabled)
		if err != nil {
			return nil, err
		}
		if err := decodeZonePoints(&z, pointsJSON); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(labelsJSON), &z.Labels); err != nil {
			slog.Warn("corrupted zone labels JSON", "zone", z.Name, "camera", z.Camera, "error", err)
			z.Labels = nil
		}
		zones = append(zones, z)
	}
	return zones, rows.Err()
}

func scanZone(row *sql.Row) (*camera.Zone, error) {
	var z camera.Zone
	var pointsJSON, labelsJSON string
	err := row.Scan(&z.ID, &z.Camera, &z.Name, &pointsJSON, &z.X1, &z.Y1, &z.X2, &z.Y2,
		&labelsJSON, &z.TrackPresence, &z.FaceRecognition, &z.Enabled)
	if err != nil {
		return nil, err
	}
	if err := decodeZonePoints(&z, pointsJSON); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(labelsJSON), &z.Labels); err != nil {
		slog.Warn("corrupted zone labels JSON", "zone", z.Name, "camera", z.Camera, "error", err)
		z.Labels = nil
	}
	return &z, nil
}

// --- People & Face operations ---

// Person represents a known or unknown person in the face recognition system.
type Person struct {
	ID            int64     `json:"id"`
	Name          string    `json:"name"`
	Ignore        bool      `json:"ignore"`
	Centroid      []byte    `json:"-"`
	SourceEventID string    `json:"source_event_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// Face represents a detected face with its embedding and metadata.
type Face struct {
	ID         int64     `json:"id"`
	EventID    string    `json:"event_id"`
	Camera     string    `json:"camera"`
	PersonID   *int64    `json:"person_id"`
	Embedding  []byte    `json:"-"`
	CropPath   string    `json:"crop_path,omitempty"`
	Confidence float64   `json:"confidence"`
	Similarity *float64  `json:"similarity"`
	Timestamp  time.Time `json:"timestamp"`
	CreatedAt  time.Time `json:"created_at"`
}

// SavePerson creates a new person record and returns the assigned ID.
func (d *DB) SavePerson(name string, ignore bool, centroid []byte) (int64, error) {
	return d.SavePersonWithEvent(name, ignore, centroid, "")
}

func (d *DB) SavePersonWithEvent(name string, ignore bool, centroid []byte, sourceEventID string) (int64, error) {
	result, err := d.db.Exec(
		"INSERT INTO people (name, ignore, centroid, source_event_id) VALUES (?, ?, ?, ?)",
		name, ignore, centroid, nullString(sourceEventID),
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// GetPerson returns a person by ID, or nil if not found.
func (d *DB) GetPerson(id int64) (*Person, error) {
	row := d.db.QueryRow(
		"SELECT id, name, ignore, centroid, source_event_id, created_at FROM people WHERE id = ?", id)

	var p Person
	var name, sourceEventID sql.NullString
	var centroid []byte
	err := row.Scan(&p.ID, &name, &p.Ignore, &centroid, &sourceEventID, &p.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.Name = name.String
	p.Centroid = centroid
	p.SourceEventID = sourceEventID.String
	return &p, nil
}

// ListPeople returns all people ordered by name.
func (d *DB) ListPeople() ([]Person, error) {
	rows, err := d.db.Query(
		"SELECT id, name, ignore, centroid, source_event_id, created_at FROM people ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var people []Person
	for rows.Next() {
		var p Person
		var name, sourceEventID sql.NullString
		var centroid []byte
		if err := rows.Scan(&p.ID, &name, &p.Ignore, &centroid, &sourceEventID, &p.CreatedAt); err != nil {
			return nil, err
		}
		p.Name = name.String
		p.Centroid = centroid
		p.SourceEventID = sourceEventID.String
		people = append(people, p)
	}
	return people, rows.Err()
}

// UpdatePersonCentroid updates the centroid embedding for a person.
func (d *DB) UpdatePersonCentroid(id int64, centroid []byte) error {
	_, err := d.db.Exec("UPDATE people SET centroid = ? WHERE id = ?", centroid, id)
	return err
}

// UpdatePersonName updates the name for a person.
func (d *DB) UpdatePersonName(id int64, name string) error {
	_, err := d.db.Exec("UPDATE people SET name = ? WHERE id = ?", name, id)
	return err
}

// SaveFace inserts a face record and returns the assigned ID.
func (d *DB) SaveFace(face Face) (int64, error) {
	result, err := d.db.Exec(`
		INSERT INTO faces (event_id, camera, person_id, embedding, crop_path, confidence, similarity, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		nullString(face.EventID), face.Camera, face.PersonID,
		face.Embedding, nullString(face.CropPath),
		face.Confidence, face.Similarity, utc(face.Timestamp),
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// ListFacesByPerson returns all faces assigned to a given person, ordered by timestamp descending.
func (d *DB) ListFacesByPerson(personID int64, limit int) ([]Face, error) {
	query := `
		SELECT id, event_id, camera, person_id, embedding, crop_path, confidence, similarity, timestamp, created_at
		FROM faces WHERE person_id = ?
		ORDER BY timestamp DESC`
	args := []any{personID}

	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return scanFaces(rows)
}

// ListUnmatchedFaces returns faces without a person assignment.
func (d *DB) ListUnmatchedFaces(limit int) ([]Face, error) {
	query := `
		SELECT id, event_id, camera, person_id, embedding, crop_path, confidence, similarity, timestamp, created_at
		FROM faces WHERE person_id IS NULL
		ORDER BY timestamp DESC`
	args := []any{}

	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return scanFaces(rows)
}

// UpdateFacePerson assigns a face to a person with the given similarity score.
func (d *DB) UpdateFacePerson(faceID, personID int64, similarity float64) error {
	_, err := d.db.Exec(
		"UPDATE faces SET person_id = ?, similarity = ? WHERE id = ?",
		personID, similarity, faceID,
	)
	return err
}

// DeletePerson removes a person record by ID.
func (d *DB) DeletePerson(id int64) error {
	_, err := d.db.Exec("DELETE FROM people WHERE id = ?", id)
	return err
}

// SetPersonIgnore updates the ignore flag for a person.
func (d *DB) SetPersonIgnore(id int64, ignore bool) error {
	_, err := d.db.Exec("UPDATE people SET ignore = ? WHERE id = ?", ignore, id)
	return err
}

// MergePeople merges person sourceID into targetID: reassigns all faces and deletes the source person.
func (d *DB) MergePeople(targetID, sourceID int64) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Reassign all faces from source to target
	if _, err := tx.Exec("UPDATE faces SET person_id = ? WHERE person_id = ?", targetID, sourceID); err != nil {
		return err
	}
	// Delete the source person
	if _, err := tx.Exec("DELETE FROM people WHERE id = ?", sourceID); err != nil {
		return err
	}
	return tx.Commit()
}

// GetFaceCropPath returns the crop_path for a face by ID.
func (d *DB) DeleteFace(id int64) error {
	_, err := d.db.Exec("DELETE FROM faces WHERE id = ?", id)
	return err
}

func (d *DB) GetFaceCropPath(id int64) (string, error) {
	var path sql.NullString
	err := d.db.QueryRow("SELECT crop_path FROM faces WHERE id = ?", id).Scan(&path)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return path.String, nil
}

// FaceEventIDs returns the distinct event IDs that already have face records.
func (d *DB) FaceEventIDs() ([]string, error) {
	rows, err := d.db.Query("SELECT DISTINCT event_id FROM faces WHERE event_id IS NOT NULL AND event_id != ''")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

type AuthSession struct {
	ID         string
	Username   string
	CSRFToken  string
	RemoteIP   string
	UserAgent  string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	IdleTTL    time.Duration
}

type APIToken struct {
	ID          int64     `json:"id"`
	Username    string    `json:"username"`
	Name        string    `json:"name"`
	TokenPrefix string    `json:"token_prefix"`
	Scopes      []string  `json:"scopes"`
	CreatedAt   time.Time `json:"created_at"`
	LastUsedAt  time.Time `json:"last_used_at,omitempty"`
	RevokedAt   time.Time `json:"revoked_at,omitempty"`
	TokenHash   []byte    `json:"-"`
}

func (d *DB) CreateSession(session AuthSession) error {
	idleSecs := int64(session.IdleTTL.Seconds())
	if idleSecs <= 0 {
		idleSecs = 1800 // default 30 minutes
	}
	_, err := d.db.Exec(`
		INSERT INTO auth_sessions (id, username, csrf_token, remote_ip, user_agent, created_at, last_seen_at, expires_at, idle_ttl_seconds)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		session.ID, session.Username, session.CSRFToken, nullString(session.RemoteIP), nullString(session.UserAgent),
		utc(session.CreatedAt), utc(session.LastSeenAt), utc(session.ExpiresAt), idleSecs,
	)
	return err
}

func (d *DB) GetSession(id string) (*AuthSession, error) {
	row := d.db.QueryRow(`
		SELECT id, username, csrf_token, remote_ip, user_agent, created_at, last_seen_at, expires_at, idle_ttl_seconds
		FROM auth_sessions WHERE id = ?`, id)
	var session AuthSession
	var remoteIP, userAgent sql.NullString
	var idleSecs int64
	err := row.Scan(&session.ID, &session.Username, &session.CSRFToken, &remoteIP, &userAgent, &session.CreatedAt, &session.LastSeenAt, &session.ExpiresAt, &idleSecs)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	session.RemoteIP = remoteIP.String
	session.UserAgent = userAgent.String
	session.IdleTTL = time.Duration(idleSecs) * time.Second
	return &session, nil
}

func (d *DB) TouchSession(id string, lastSeen time.Time) error {
	_, err := d.db.Exec("UPDATE auth_sessions SET last_seen_at = ? WHERE id = ?", utc(lastSeen), id)
	return err
}

func (d *DB) DeleteSession(id string) error {
	_, err := d.db.Exec("DELETE FROM auth_sessions WHERE id = ?", id)
	return err
}

func (d *DB) DeleteExpiredSessions(now time.Time) error {
	_, err := d.db.Exec(`DELETE FROM auth_sessions
		WHERE expires_at <= ?
		   OR (julianday(?) - julianday(last_seen_at)) * 86400 > idle_ttl_seconds`,
		utc(now), utc(now))
	return err
}

func (d *DB) CreateAPIToken(token APIToken) (int64, error) {
	scopesJSON, err := json.Marshal(token.Scopes)
	if err != nil {
		return 0, err
	}
	result, err := d.db.Exec(`
		INSERT INTO api_tokens (username, name, token_prefix, token_hash, scopes, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		token.Username, token.Name, token.TokenPrefix, token.TokenHash, string(scopesJSON), utc(token.CreatedAt),
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (d *DB) GetAPITokenByHash(hash []byte) (*APIToken, error) {
	row := d.db.QueryRow(`
		SELECT id, username, name, token_prefix, token_hash, scopes, created_at, last_used_at, revoked_at
		FROM api_tokens WHERE token_hash = ?`, hash)
	var token APIToken
	var scopesJSON string
	var lastUsedAt, revokedAt sql.NullTime
	err := row.Scan(&token.ID, &token.Username, &token.Name, &token.TokenPrefix, &token.TokenHash, &scopesJSON, &token.CreatedAt, &lastUsedAt, &revokedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(scopesJSON), &token.Scopes); err != nil {
		return nil, err
	}
	if lastUsedAt.Valid {
		token.LastUsedAt = lastUsedAt.Time
	}
	if revokedAt.Valid {
		token.RevokedAt = revokedAt.Time
	}
	return &token, nil
}

func (d *DB) TouchAPIToken(id int64, lastUsed time.Time) error {
	_, err := d.db.Exec("UPDATE api_tokens SET last_used_at = ? WHERE id = ?", utc(lastUsed), id)
	return err
}

func (d *DB) RevokeAPIToken(id int64, username string) error {
	_, err := d.db.Exec("UPDATE api_tokens SET revoked_at = ? WHERE id = ? AND username = ?", utc(time.Now()), id, username)
	return err
}

// --- Auth User operations ---

// AuthUser represents a user stored in the database for authentication.
type AuthUser struct {
	Username     string
	PasswordHash string
}

// SaveAuthUser creates or updates an auth user. If the username already exists,
// the password hash and updated_at timestamp are overwritten.
func (d *DB) SaveAuthUser(username, passwordHash string) error {
	_, err := d.db.Exec(`
		INSERT INTO auth_users (username, password_hash)
		VALUES (?, ?)
		ON CONFLICT(username) DO UPDATE SET
			password_hash = excluded.password_hash,
			updated_at = CURRENT_TIMESTAMP`,
		username, passwordHash,
	)
	return err
}

// SeedAuthUser inserts a user only if the username does not already exist.
// This preserves any password changes made through the UI/API.
func (d *DB) SeedAuthUser(username, passwordHash string) error {
	_, err := d.db.Exec(`INSERT OR IGNORE INTO auth_users (username, password_hash) VALUES (?, ?)`,
		username, passwordHash,
	)
	return err
}

// ListAuthUsers returns all auth users ordered by username.
func (d *DB) ListAuthUsers() ([]AuthUser, error) {
	rows, err := d.db.Query("SELECT username, password_hash FROM auth_users ORDER BY username")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var users []AuthUser
	for rows.Next() {
		var u AuthUser
		if err := rows.Scan(&u.Username, &u.PasswordHash); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func scanFaces(rows *sql.Rows) ([]Face, error) {
	var faces []Face
	for rows.Next() {
		var f Face
		var eventID, cropPath sql.NullString
		var personID sql.NullInt64
		var similarity sql.NullFloat64
		err := rows.Scan(&f.ID, &eventID, &f.Camera, &personID,
			&f.Embedding, &cropPath, &f.Confidence, &similarity,
			&f.Timestamp, &f.CreatedAt)
		if err != nil {
			return nil, err
		}
		f.EventID = eventID.String
		f.CropPath = cropPath.String
		if personID.Valid {
			pid := personID.Int64
			f.PersonID = &pid
		}
		if similarity.Valid {
			sim := similarity.Float64
			f.Similarity = &sim
		}
		faces = append(faces, f)
	}
	return faces, rows.Err()
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func zoneBounds(points [][]float64) (x1, y1, x2, y2 float64) {
	if len(points) == 0 {
		return 0, 0, 0, 0
	}
	x1, y1 = points[0][0], points[0][1]
	x2, y2 = x1, y1
	for _, point := range points[1:] {
		if len(point) != 2 {
			continue
		}
		if point[0] < x1 {
			x1 = point[0]
		}
		if point[1] < y1 {
			y1 = point[1]
		}
		if point[0] > x2 {
			x2 = point[0]
		}
		if point[1] > y2 {
			y2 = point[1]
		}
	}
	return x1, y1, x2, y2
}

// KnownObject represents a user-defined object to recognize (e.g. "Ruben's car").
type KnownObject struct {
	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	Label          string   `json:"label"`
	Centroid       []byte   `json:"-"`
	CropPath       string   `json:"crop_path,omitempty"`
	MatchThreshold *float64 `json:"match_threshold,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// ObjectSighting records when a known object was recognized in an event.
type ObjectSighting struct {
	ID         int64     `json:"id"`
	EventID    string    `json:"event_id"`
	Camera     string    `json:"camera"`
	ObjectID   int64     `json:"object_id"`
	ObjectName string    `json:"object_name,omitempty"`
	Similarity float64   `json:"similarity"`
	Timestamp  time.Time `json:"timestamp"`
}

type ObjectReference struct {
	ID        int64     `json:"id"`
	ObjectID  int64     `json:"object_id"`
	EventID   string    `json:"event_id,omitempty"`
	Embedding []byte    `json:"-"`
	CropPath  string    `json:"crop_path,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func (d *DB) SaveKnownObject(obj KnownObject) (int64, error) {
	result, err := d.db.Exec(`
		INSERT INTO known_objects (name, label, centroid, crop_path, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		obj.Name, obj.Label, obj.Centroid, nullString(obj.CropPath), utc(time.Now()),
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (d *DB) UpdateKnownObjectCrop(id int64, cropPath string) error {
	_, err := d.db.Exec("UPDATE known_objects SET crop_path = ? WHERE id = ?", cropPath, id)
	return err
}

func (d *DB) UpdateKnownObjectName(id int64, name string) error {
	_, err := d.db.Exec("UPDATE known_objects SET name = ? WHERE id = ?", name, id)
	return err
}

func (d *DB) UpdateKnownObjectThreshold(id int64, threshold *float64) error {
	_, err := d.db.Exec("UPDATE known_objects SET match_threshold = ? WHERE id = ?", threshold, id)
	return err
}

func (d *DB) DeleteObjectSighting(id int64) error {
	_, err := d.db.Exec("DELETE FROM object_sightings WHERE id = ?", id)
	return err
}

func (d *DB) GetObjectSighting(id int64) (*ObjectSighting, error) {
	row := d.db.QueryRow(`SELECT s.id, s.event_id, s.camera, s.object_id, o.name, s.similarity, s.timestamp
		FROM object_sightings s JOIN known_objects o ON s.object_id = o.id WHERE s.id = ?`, id)
	var s ObjectSighting
	var eventID sql.NullString
	err := row.Scan(&s.ID, &eventID, &s.Camera, &s.ObjectID, &s.ObjectName, &s.Similarity, &s.Timestamp)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.EventID = eventID.String
	return &s, nil
}

func (d *DB) UpdateKnownObjectCentroid(id int64, centroid []byte) error {
	_, err := d.db.Exec("UPDATE known_objects SET centroid = ? WHERE id = ?", centroid, id)
	return err
}

func (d *DB) UpdateEventObjectName(eventID, objectName string) error {
	_, err := d.db.Exec("UPDATE events SET object_name = ? WHERE id = ?", objectName, eventID)
	return err
}

func (d *DB) UpdateEventSubLabel(eventID, subLabel string) error {
	_, err := d.db.Exec("UPDATE events SET sub_label = ? WHERE id = ?", subLabel, eventID)
	return err
}

func (d *DB) UpdateSubLabelsForPerson(personID int64, name string) error {
	_, err := d.db.Exec(`
		UPDATE events SET sub_label = ?
		WHERE id IN (SELECT event_id FROM faces WHERE person_id = ? AND event_id IS NOT NULL AND event_id != '')`,
		name, personID)
	return err
}

func (d *DB) RecentUnmatchedEventsByLabel(label string, limit int) ([]camera.Event, error) {
	rows, err := d.db.Query(`
		SELECT id, camera, label, score, box_x1, box_y1, box_x2, box_y2, timestamp, end_time,
			snapshot_path, snapshot_available, clip_path, clip_available, zone_name, object_name, sub_label
		FROM events
		WHERE label = ? AND snapshot_available = 1
			AND (object_name IS NULL OR object_name = '')
		ORDER BY timestamp DESC LIMIT ?`, label, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanEvents(rows)
}

func (d *DB) ListKnownObjects() ([]KnownObject, error) {
	rows, err := d.db.Query(`SELECT id, name, label, centroid, crop_path, match_threshold, created_at FROM known_objects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanKnownObjects(rows)
}

func (d *DB) ListKnownObjectsByLabel(label string) ([]KnownObject, error) {
	rows, err := d.db.Query(`SELECT id, name, label, centroid, crop_path, match_threshold, created_at FROM known_objects WHERE label = ? ORDER BY name`, label)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanKnownObjects(rows)
}

func (d *DB) GetKnownObject(id int64) (*KnownObject, error) {
	row := d.db.QueryRow(`SELECT id, name, label, centroid, crop_path, match_threshold, created_at FROM known_objects WHERE id = ?`, id)
	var obj KnownObject
	var cropPath sql.NullString
	var threshold sql.NullFloat64
	err := row.Scan(&obj.ID, &obj.Name, &obj.Label, &obj.Centroid, &cropPath, &threshold, &obj.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	obj.CropPath = cropPath.String
	if threshold.Valid {
		obj.MatchThreshold = &threshold.Float64
	}
	return &obj, nil
}

func (d *DB) DeleteKnownObject(id int64) error {
	_, err := d.db.Exec("DELETE FROM known_objects WHERE id = ?", id)
	return err
}

func (d *DB) SaveObjectSighting(s ObjectSighting) (int64, error) {
	result, err := d.db.Exec(`
		INSERT INTO object_sightings (event_id, camera, object_id, similarity, timestamp)
		VALUES (?, ?, ?, ?, ?)`,
		s.EventID, s.Camera, s.ObjectID, s.Similarity, utc(s.Timestamp),
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (d *DB) ListObjectSightings(objectID int64, limit int) ([]ObjectSighting, error) {
	query := `SELECT s.id, s.event_id, s.camera, s.object_id, o.name, s.similarity, s.timestamp
		FROM object_sightings s JOIN known_objects o ON s.object_id = o.id
		WHERE s.object_id = ? ORDER BY s.timestamp DESC`
	args := []any{objectID}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanObjectSightings(rows)
}

func (d *DB) GetEventSightings(eventID string) ([]ObjectSighting, error) {
	rows, err := d.db.Query(`
		SELECT s.id, s.event_id, s.camera, s.object_id, o.name, s.similarity, s.timestamp
		FROM object_sightings s JOIN known_objects o ON s.object_id = o.id
		WHERE s.event_id = ? ORDER BY s.similarity DESC`, eventID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanObjectSightings(rows)
}

func scanKnownObjects(rows *sql.Rows) ([]KnownObject, error) {
	var objects []KnownObject
	for rows.Next() {
		var obj KnownObject
		var cropPath sql.NullString
		var threshold sql.NullFloat64
		if err := rows.Scan(&obj.ID, &obj.Name, &obj.Label, &obj.Centroid, &cropPath, &threshold, &obj.CreatedAt); err != nil {
			return nil, err
		}
		obj.CropPath = cropPath.String
		if threshold.Valid {
			obj.MatchThreshold = &threshold.Float64
		}
		objects = append(objects, obj)
	}
	return objects, rows.Err()
}

func scanObjectSightings(rows *sql.Rows) ([]ObjectSighting, error) {
	var sightings []ObjectSighting
	for rows.Next() {
		var s ObjectSighting
		var eventID sql.NullString
		if err := rows.Scan(&s.ID, &eventID, &s.Camera, &s.ObjectID, &s.ObjectName, &s.Similarity, &s.Timestamp); err != nil {
			return nil, err
		}
		s.EventID = eventID.String
		sightings = append(sightings, s)
	}
	return sightings, rows.Err()
}

func (d *DB) SaveObjectReference(ref ObjectReference) (int64, error) {
	result, err := d.db.Exec(`
		INSERT INTO object_references (object_id, event_id, embedding, crop_path, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		ref.ObjectID, nullString(ref.EventID), ref.Embedding, nullString(ref.CropPath), utc(time.Now()),
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (d *DB) ListObjectReferences(objectID int64) ([]ObjectReference, error) {
	rows, err := d.db.Query(`SELECT id, object_id, event_id, embedding, crop_path, created_at
		FROM object_references WHERE object_id = ? ORDER BY created_at`, objectID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var refs []ObjectReference
	for rows.Next() {
		var r ObjectReference
		var eventID, cropPath sql.NullString
		if err := rows.Scan(&r.ID, &r.ObjectID, &eventID, &r.Embedding, &cropPath, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.EventID = eventID.String
		r.CropPath = cropPath.String
		refs = append(refs, r)
	}
	return refs, rows.Err()
}

func (d *DB) DeleteObjectReference(id int64) error {
	_, err := d.db.Exec("DELETE FROM object_references WHERE id = ?", id)
	return err
}

func (d *DB) CountObjectReferences(objectID int64) (int, error) {
	var count int
	err := d.db.QueryRow("SELECT COUNT(*) FROM object_references WHERE object_id = ?", objectID).Scan(&count)
	return count, err
}

func decodeZonePoints(z *camera.Zone, pointsJSON string) error {
	if pointsJSON != "" && pointsJSON != "[]" {
		if err := json.Unmarshal([]byte(pointsJSON), &z.Points); err != nil {
			return fmt.Errorf("unmarshal zone points: %w", err)
		}
		return nil
	}
	z.Points = [][]float64{
		{z.X1, z.Y1},
		{z.X2, z.Y1},
		{z.X2, z.Y2},
		{z.X1, z.Y2},
	}
	return nil
}
