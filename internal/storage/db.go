package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/rvben/vedetta/internal/camera"
	_ "modernc.org/sqlite"
)

// SegmentRecord represents a recorded video segment stored in the database.
type SegmentRecord struct {
	ID                 int64
	Camera             string
	Path               string
	StartTime          time.Time
	EndTime            time.Time
	SizeBytes          int64
	Recompressed       bool
	RecompressedAt     time.Time
	RecompressFailures int
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
	category := event.Category
	if category == "" {
		category = camera.CategoryAlert
	}
	kind := event.Kind
	if kind == "" {
		kind = camera.EventKindObject
	}
	var answeredAt *time.Time
	if !event.AnsweredAt.IsZero() {
		t := utc(event.AnsweredAt)
		answeredAt = &t
	}
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.Exec(`
		INSERT INTO events (id, camera, label, score, box_x1, box_y1, box_x2, box_y2, timestamp, end_time, snapshot_path, snapshot_available, clip_path, clip_available, zone_name, object_name, sub_label, category, kind, answered_at, answered_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID, event.CameraName, event.Label, event.Score,
		event.Box[0], event.Box[1], event.Box[2], event.Box[3],
		utc(event.Timestamp), endTime, event.SnapshotPath, event.SnapshotAvailable, event.ClipPath, event.ClipAvailable, zoneName, nullString(event.ObjectName), nullString(event.SubLabel), category,
		kind, answeredAt, nullString(event.AnsweredBy),
	)
	if err != nil {
		return err
	}
	if err := assignEventToActivityTx(tx, event); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) UpdateEventEndTime(eventID string, endTime time.Time) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("UPDATE events SET end_time = ? WHERE id = ?", utc(endTime), eventID); err != nil {
		return err
	}
	var activityID string
	err = tx.QueryRow(`SELECT activity_id FROM activity_events WHERE event_id = ?`, eventID).Scan(&activityID)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if activityID != "" {
		survivingID, err := reconcileActivityTx(tx, activityID)
		if err != nil {
			return err
		}
		if survivingID != "" {
			if err := reopenActivityTx(tx, survivingID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// UpdateEventAnswered records that a doorbell ring was acknowledged. It only sets
// the columns while answered_at IS NULL, so the first answerer wins under
// concurrent calls.
func (d *DB) UpdateEventAnswered(eventID string, answeredAt time.Time, answeredBy string) error {
	_, err := d.db.Exec(
		"UPDATE events SET answered_at = ?, answered_by = ? WHERE id = ? AND answered_at IS NULL",
		utc(answeredAt), answeredBy, eventID,
	)
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

// SetEventClip records a freshly written clip on its event row: it stores the
// clip path, marks the clip available, records its on-disk size, and RESETS the
// recompression state. The reset is what makes re-extraction correct: a fresh
// full-resolution clip must start un-recompressed even if the prior clip on
// this row had been recompressed.
func (d *DB) SetEventClip(eventID, clipPath string, sizeBytes int64) error {
	_, err := d.db.Exec(`
		UPDATE events
		SET clip_path = ?, clip_available = TRUE, clip_size_bytes = ?,
		    recompressed = FALSE, recompressed_at = NULL, recompress_failures = 0
		WHERE id = ?`,
		clipPath, sizeBytes, eventID,
	)
	return err
}

// ClearEventClip drops the clip reference after the clip file is deleted: it
// blanks the path, marks the clip unavailable, zeroes the recorded size, and
// resets the recompression state so a cleared row never appears eligible and
// carries no stale recompression flags.
func (d *DB) ClearEventClip(eventID string) error {
	_, err := d.db.Exec(`
		UPDATE events
		SET clip_path = '', clip_available = FALSE, clip_size_bytes = 0,
		    recompressed = FALSE, recompressed_at = NULL, recompress_failures = 0
		WHERE id = ?`,
		eventID,
	)
	return err
}

// UpdateEventMediaAvailability sets both media-availability flags in a single
// row write. Used by reconciliation, which recomputes both at once.
func (d *DB) UpdateEventMediaAvailability(eventID string, snapshotAvailable, clipAvailable bool) error {
	_, err := d.db.Exec(
		"UPDATE events SET snapshot_available = ?, clip_available = ? WHERE id = ?",
		snapshotAvailable, clipAvailable, eventID,
	)
	return err
}

// EventFilters narrows event queries. Empty fields are ignored.
type EventFilters struct {
	Camera   string
	Label    string
	Zone     string
	Object   string
	Category string // "alert" or "detection"; empty matches all
	Kind     string // "object" or "doorbell"; empty matches all
	Search   string // free-text LIKE across camera, label, object_name, sub_label
	// After is an inclusive lower bound on the timestamp: a range that starts
	// at midnight has to contain an event at midnight. Since is the exclusive
	// form, for a caller polling with the timestamp of the newest event it
	// already holds, which must not come back again on every poll.
	After  time.Time
	Since  time.Time
	Before time.Time
}

// QueryEvents returns events matching the given filters.
func (d *DB) QueryEvents(cameraName, label string, limit, offset int) ([]camera.Event, error) {
	return d.QueryEventsFiltered(EventFilters{Camera: cameraName, Label: label}, limit, offset)
}

// QueryEventsFiltered returns events matching all given filters.
func (d *DB) QueryEventsFiltered(f EventFilters, limit, offset int) ([]camera.Event, error) {
	where, args := eventFilterClause(f)
	query := "SELECT " + eventSelectCols + " FROM events" + where + " ORDER BY timestamp DESC"

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

// LatestSnapshotEvent returns the newest event for a camera whose snapshot is
// both marked available and backed by a non-empty path. Push test notifications
// use it to show real evidence without exposing an authenticated media URL.
func (d *DB) LatestSnapshotEvent(cameraName string) (*camera.Event, error) {
	rows, err := d.db.Query(`
		SELECT `+eventSelectCols+`
		FROM events
		WHERE camera = ?
		  AND snapshot_available = 1
		  AND TRIM(COALESCE(snapshot_path, '')) != ''
		ORDER BY timestamp DESC, id DESC
		LIMIT 1`, cameraName)
	if err != nil {
		return nil, err
	}
	events, err := scanEvents(rows)
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, nil
	}
	return &events[0], nil
}

// CountEventsFiltered returns the total count of events matching the given filters.
func (d *DB) CountEventsFiltered(f EventFilters) (int, error) {
	where, args := eventFilterClause(f)
	var count int
	err := d.db.QueryRow("SELECT COUNT(*) FROM events"+where, args...).Scan(&count)
	return count, err
}

func eventFilterClause(f EventFilters) (string, []any) {
	clauses := []string{"1=1"}
	args := []any{}
	if f.Camera != "" {
		clauses = append(clauses, "camera = ?")
		args = append(args, f.Camera)
	}
	if f.Label != "" {
		clauses = append(clauses, "label = ?")
		args = append(args, f.Label)
	}
	if f.Zone != "" {
		clauses = append(clauses, "zone_name = ?")
		args = append(args, f.Zone)
	}
	if f.Object != "" {
		clauses = append(clauses, "(object_name = ? OR sub_label = ?)")
		args = append(args, f.Object, f.Object)
	}
	if f.Category != "" {
		clauses = append(clauses, "category = ?")
		args = append(args, f.Category)
	}
	if f.Kind != "" {
		clauses = append(clauses, "kind = ?")
		args = append(args, f.Kind)
	}
	if q := strings.TrimSpace(f.Search); q != "" {
		like := "%" + q + "%"
		clauses = append(clauses, "(camera LIKE ? OR label LIKE ? OR IFNULL(object_name,'') LIKE ? OR IFNULL(sub_label,'') LIKE ?)")
		args = append(args, like, like, like, like)
	}
	// Every timestamp bind goes through utc(): the column holds canonical UTC
	// text, so a time.Time carrying a zone offset would be serialized with
	// that offset and compared as a different string.
	if !f.After.IsZero() {
		clauses = append(clauses, "timestamp >= ?")
		args = append(args, utc(f.After))
	}
	if !f.Since.IsZero() {
		clauses = append(clauses, "timestamp > ?")
		args = append(args, utc(f.Since))
	}
	if !f.Before.IsZero() {
		clauses = append(clauses, "timestamp < ?")
		args = append(args, utc(f.Before))
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// CountEventsByLabel returns the count of events grouped by label.
// When kind is non-empty, only events of that kind are counted.
func (d *DB) CountEventsByLabel(kind string) (map[string]int, error) {
	q := "SELECT label, COUNT(*) FROM events"
	var args []any
	if kind != "" {
		q += " WHERE kind = ?"
		args = append(args, kind)
	}
	q += " GROUP BY label"
	rows, err := d.db.Query(q, args...)
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
// When kind is non-empty, only events of that kind are counted.
func (d *DB) CountEventsByCamera(kind string) (map[string]int, error) {
	q := "SELECT camera, COUNT(*) FROM events"
	var args []any
	if kind != "" {
		q += " WHERE kind = ?"
		args = append(args, kind)
	}
	q += " GROUP BY camera"
	rows, err := d.db.Query(q, args...)
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
// When kind is non-empty, only events of that kind are counted.
func (d *DB) CountEvents(kind string) (int, error) {
	q := "SELECT COUNT(*) FROM events"
	var args []any
	if kind != "" {
		q += " WHERE kind = ?"
		args = append(args, kind)
	}
	var count int
	err := d.db.QueryRow(q, args...).Scan(&count)
	return count, err
}

// utc normalizes a time.Time to UTC and strips the monotonic clock reading.
// This ensures consistent text representation in SQLite for correct comparisons.
func utc(t time.Time) time.Time {
	return t.UTC().Round(0)
}

// Timestamp columns are stored in the driver's canonical UTC text form (the v2
// migration guarantees this), so timestamp predicates compare the column
// directly against a utc()-bound time.Time. No replace() wrapper is used: it
// would force a full table scan instead of an index search. These constants
// hold the exact SQL the corresponding methods run so the query-plan tests can
// assert index usage against the production statement.
const (
	sqlSegmentsOverlappingByCamera = `
		SELECT id, camera, path, start_time, end_time, size_bytes, recompressed, recompressed_at, recompress_failures
		FROM segments
		WHERE camera = ? AND start_time < ? AND end_time > ?
		ORDER BY start_time`

	sqlSegmentsOverlappingAll = `
		SELECT id, camera, path, start_time, end_time, size_bytes, recompressed, recompressed_at, recompress_failures
		FROM segments
		WHERE start_time < ? AND end_time > ?
		ORDER BY start_time`

	sqlEventsInRangeByCamera = `
		SELECT id, camera, label, score, box_x1, box_y1, box_x2, box_y2, timestamp, end_time, snapshot_path, snapshot_available, clip_path, clip_available, zone_name, object_name, sub_label, category, kind, answered_at, answered_by
		FROM events
		WHERE camera = ? AND timestamp >= ? AND timestamp < ?
		ORDER BY timestamp`

	sqlSegmentsEndingBefore = `
		SELECT id, camera, path, start_time, end_time, size_bytes, recompressed, recompressed_at, recompress_failures
		FROM segments
		WHERE end_time < ?
		ORDER BY end_time ASC`
)

// SaveSegment inserts or updates a segment record in the database.
// On conflict with an existing path, only the mutable fields (start_time, end_time,
// size_bytes) are updated — recompression state is preserved.
func (d *DB) SaveSegment(seg SegmentRecord) error {
	_, err := d.db.Exec(`
		INSERT INTO segments (camera, path, start_time, end_time, size_bytes)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			start_time = excluded.start_time,
			end_time   = excluded.end_time,
			size_bytes = excluded.size_bytes`,
		seg.Camera, seg.Path, utc(seg.StartTime), utc(seg.EndTime), seg.SizeBytes,
	)
	return err
}

// SaveMotionActivity inserts or replaces a motion activity score for a camera and minute bucket.
func (d *DB) SaveMotionActivity(camera string, bucket time.Time, score float64) error {
	_, err := d.db.Exec("INSERT OR REPLACE INTO motion_activity (camera, bucket, score) VALUES (?, ?, ?)", camera, utc(bucket), score)
	return err
}

// GetMotionActivityInRange returns motion buckets for a camera within [start, end).
func (d *DB) GetMotionActivityInRange(camera string, start, end time.Time) ([]MotionBucket, error) {
	rows, err := d.db.Query("SELECT bucket, score FROM motion_activity WHERE camera = ? AND bucket >= ? AND bucket < ? ORDER BY bucket",
		camera, utc(start), utc(end))
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
	return d.GetSegmentsOverlapping(cameraName, from, to)
}

// DeleteSegment removes a segment record by path.
func (d *DB) DeleteSegment(path string) error {
	_, err := d.db.Exec("DELETE FROM segments WHERE path = ?", path)
	return err
}

// DeleteSegmentByID removes a segment row by primary key.
func (d *DB) DeleteSegmentByID(id int64) error {
	_, err := d.db.Exec(`DELETE FROM segments WHERE id = ?`, id)
	return err
}

// GetAllSegments returns all segment records for a given camera.
func (d *DB) GetAllSegments(cameraName string) ([]SegmentRecord, error) {
	rows, err := d.db.Query(`
		SELECT id, camera, path, start_time, end_time, size_bytes, recompressed, recompressed_at, recompress_failures
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
		SELECT id, camera, path, start_time, end_time, size_bytes, recompressed, recompressed_at, recompress_failures
		FROM segments WHERE path = ?`, path)

	var seg SegmentRecord
	var recompressedAt sql.NullTime
	err := row.Scan(&seg.ID, &seg.Camera, &seg.Path, &seg.StartTime, &seg.EndTime, &seg.SizeBytes,
		&seg.Recompressed, &recompressedAt, &seg.RecompressFailures)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if recompressedAt.Valid {
		seg.RecompressedAt = recompressedAt.Time
	}
	return &seg, nil
}

// GetSegmentByID returns a single segment record by its primary key, or nil if not found.
func (d *DB) GetSegmentByID(id int64) (*SegmentRecord, error) {
	row := d.db.QueryRow(
		`SELECT id, camera, path, start_time, end_time, size_bytes, recompressed, recompressed_at, recompress_failures FROM segments WHERE id = ?`, id)
	var s SegmentRecord
	var recompressedAt sql.NullTime
	err := row.Scan(&s.ID, &s.Camera, &s.Path, &s.StartTime, &s.EndTime, &s.SizeBytes,
		&s.Recompressed, &recompressedAt, &s.RecompressFailures)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if recompressedAt.Valid {
		s.RecompressedAt = recompressedAt.Time
	}
	return &s, nil
}

// CountEventsToday returns the number of events with timestamp >= today midnight UTC.
// When kind is non-empty, only events of that kind are counted.
func (d *DB) CountEventsToday(kind string) (int, error) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	q := "SELECT COUNT(*) FROM events WHERE timestamp >= ?"
	args := []any{utc(today)}
	if kind != "" {
		q += " AND kind = ?"
		args = append(args, kind)
	}
	var count int
	err := d.db.QueryRow(q, args...).Scan(&count)
	return count, err
}

// GetEventByID returns a single event by ID, or nil if not found.
func (d *DB) GetEventByID(id string) (*camera.Event, error) {
	row := d.db.QueryRow(`
		SELECT id, camera, label, score, box_x1, box_y1, box_x2, box_y2, timestamp, end_time, snapshot_path, snapshot_available, clip_path, clip_available, zone_name, object_name, sub_label, category, kind, answered_at, answered_by
		FROM events WHERE id = ?`, id)

	var e camera.Event
	var endTime sql.NullTime
	var snapshot, clip, zoneName, objectName, subLabel sql.NullString
	var category string
	var snapshotAvailable, clipAvailable bool
	var kind string
	var answeredAt sql.NullTime
	var answeredBy sql.NullString
	err := row.Scan(&e.ID, &e.CameraName, &e.Label, &e.Score,
		&e.Box[0], &e.Box[1], &e.Box[2], &e.Box[3],
		&e.Timestamp, &endTime, &snapshot, &snapshotAvailable, &clip, &clipAvailable, &zoneName, &objectName, &subLabel, &category,
		&kind, &answeredAt, &answeredBy,
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
	e.Category = category
	e.ZoneName = zoneName.String
	e.Kind = kind
	if answeredAt.Valid {
		e.AnsweredAt = answeredAt.Time
	}
	e.AnsweredBy = answeredBy.String
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

// GetSegmentsOverlapping returns segments that overlap the [start, end) range.
// A segment overlaps when it intersects the range at all, so a segment that
// began before the range but runs into it is included. If cameraName is empty,
// returns segments for all cameras.
func (d *DB) GetSegmentsOverlapping(cameraName string, start, end time.Time) ([]SegmentRecord, error) {
	var rows *sql.Rows
	var err error
	if cameraName != "" {
		rows, err = d.db.Query(sqlSegmentsOverlappingByCamera, cameraName, utc(end), utc(start))
	} else {
		rows, err = d.db.Query(sqlSegmentsOverlappingAll, utc(end), utc(start))
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return scanSegments(rows)
}

// QueryEventsInRange returns events for a camera with timestamp within [start, end).
func (d *DB) QueryEventsInRange(cameraName string, start, end time.Time) ([]camera.Event, error) {
	rows, err := d.db.Query(sqlEventsInRangeByCamera, cameraName, utc(start), utc(end))
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

// GetSegmentsEndingBefore returns all segments whose end_time is before the
// given cutoff, across all cameras — including cameras that no longer exist
// in the current config. Used by retention cleanup to catch orphaned segments
// that filesystem-based iteration would miss.
func (d *DB) GetSegmentsEndingBefore(cutoff time.Time) ([]SegmentRecord, error) {
	rows, err := d.db.Query(sqlSegmentsEndingBefore, utc(cutoff))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return scanSegments(rows)
}

// GetSegmentsEndingBeforeForCamera is like GetSegmentsEndingBefore but scoped
// to a single camera. Used when per-camera retain_days differs from global.
func (d *DB) GetSegmentsEndingBeforeForCamera(camera string, cutoff time.Time) ([]SegmentRecord, error) {
	rows, err := d.db.Query(`
		SELECT id, camera, path, start_time, end_time, size_bytes, recompressed, recompressed_at, recompress_failures
		FROM segments
		WHERE camera = ? AND end_time < ?
		ORDER BY end_time ASC`,
		camera,
		utc(cutoff),
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanSegments(rows)
}

// GetOldestSegments returns the N oldest segments across all cameras, ordered by start_time.
func (d *DB) GetOldestSegments(limit int) ([]SegmentRecord, error) {
	rows, err := d.db.Query(`
		SELECT id, camera, path, start_time, end_time, size_bytes, recompressed, recompressed_at, recompress_failures
		FROM segments
		ORDER BY start_time ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return scanSegments(rows)
}

// GetOldestSegmentsOlderThan returns the N oldest segments whose end_time
// predates cutoff. Used by emergency cleanup: when normal age-based
// retention is not enough, this returns the candidates least painful to
// delete (the oldest of what remains), while leaving anything younger
// than cutoff untouched as the minimum-retention safety floor.
func (d *DB) GetOldestSegmentsOlderThan(limit int, cutoff time.Time) ([]SegmentRecord, error) {
	rows, err := d.db.Query(`
		SELECT id, camera, path, start_time, end_time, size_bytes, recompressed, recompressed_at, recompress_failures
		FROM segments
		WHERE end_time < ?
		ORDER BY start_time ASC
		LIMIT ?`,
		utc(cutoff),
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanSegments(rows)
}

// GetLargestSegmentSizeSince returns the maximum size_bytes among segments
// whose start_time is after since. Used to dynamically size the disk-free
// threshold so it covers at least one full segment. Returns 0 if no segments
// match.
func (d *DB) GetLargestSegmentSizeSince(since time.Time) (int64, error) {
	var max sql.NullInt64
	err := d.db.QueryRow(`
		SELECT MAX(size_bytes) FROM segments
		WHERE start_time > ?`,
		utc(since),
	).Scan(&max)
	if err != nil {
		return 0, err
	}
	return max.Int64, nil
}

// GetRecompressionCandidatesBySize returns segments for a specific camera that
// are older than cutoff, have not been recompressed, and have fewer than 3
// failures. Results are ordered by size_bytes DESC so the largest segments are
// compressed first, maximising recovered disk space per operation.
func (d *DB) GetRecompressionCandidatesBySize(camera string, cutoff time.Time, limit int) ([]SegmentRecord, error) {
	rows, err := d.db.Query(`
		SELECT id, camera, path, start_time, end_time, size_bytes, recompressed, recompressed_at, recompress_failures
		FROM segments
		WHERE camera = ?
		  AND end_time < ?
		  AND recompressed = 0
		  AND recompress_failures < 3
		ORDER BY size_bytes DESC, start_time ASC
		LIMIT ?`,
		camera,
		utc(cutoff),
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanSegments(rows)
}

// GetSegmentsForRecompression returns segments eligible for recompression:
// not yet recompressed, fewer than 3 failures, end_time before olderThan,
// ordered oldest first.
func (d *DB) GetSegmentsForRecompression(cameraName string, olderThan time.Time) ([]SegmentRecord, error) {
	rows, err := d.db.Query(`
		SELECT id, camera, path, start_time, end_time, size_bytes, recompressed, recompressed_at, recompress_failures
		FROM segments
		WHERE camera = ?
		  AND recompressed = FALSE
		  AND recompress_failures < 3
		  AND end_time < ?
		ORDER BY end_time ASC`,
		cameraName, utc(olderThan),
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanSegments(rows)
}

// SegmentsByCameraOlderThan returns all segments for the given camera whose
// start_time is before cutoff, ordered oldest first.
func (d *DB) SegmentsByCameraOlderThan(camera string, cutoff time.Time) ([]SegmentRecord, error) {
	rows, err := d.db.Query(`
		SELECT id, camera, path, start_time, end_time, size_bytes, recompressed, recompressed_at, recompress_failures
		FROM segments
		WHERE camera = ? AND start_time < ?
		ORDER BY start_time ASC`,
		camera, utc(cutoff),
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanSegments(rows)
}

// SegmentsByCameraInRange returns all segments for the given camera whose
// start_time falls in the half-open interval [from, to), ordered oldest first.
func (d *DB) SegmentsByCameraInRange(camera string, from, to time.Time) ([]SegmentRecord, error) {
	rows, err := d.db.Query(`
		SELECT id, camera, path, start_time, end_time, size_bytes, recompressed, recompressed_at, recompress_failures
		FROM segments
		WHERE camera = ?
		  AND start_time >= ?
		  AND start_time < ?
		ORDER BY start_time ASC`,
		camera, utc(from), utc(to),
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanSegments(rows)
}

// OldestSegmentsUntilBytes returns the oldest segments across all cameras,
// accumulating until their combined size_bytes reaches targetBytes. The
// returned slice is ordered oldest start_time first and always includes the
// segment that pushed the running total to or past the target, so callers
// can be sure deleting the returned set frees at least targetBytes.
// Returns nil when targetBytes <= 0.
func (d *DB) OldestSegmentsUntilBytes(targetBytes int64) ([]SegmentRecord, error) {
	if targetBytes <= 0 {
		return nil, nil
	}
	rows, err := d.db.Query(`
		SELECT id, camera, path, start_time, end_time, size_bytes, recompressed, recompressed_at, recompress_failures
		FROM segments
		ORDER BY start_time ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []SegmentRecord
	var sum int64
	for rows.Next() {
		var seg SegmentRecord
		var recompressedAt sql.NullTime
		if err := rows.Scan(
			&seg.ID, &seg.Camera, &seg.Path,
			&seg.StartTime, &seg.EndTime, &seg.SizeBytes,
			&seg.Recompressed, &recompressedAt, &seg.RecompressFailures,
		); err != nil {
			return nil, err
		}
		if recompressedAt.Valid {
			seg.RecompressedAt = recompressedAt.Time
		}
		out = append(out, seg)
		sum += seg.SizeBytes
		if sum >= targetBytes {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// excludeKinds returns the SQL fragment and arguments that keep rows whose last
// recorded failure kind is one the caller named permanent. An empty list yields
// no fragment: "nothing is permanent" is a valid policy, and an IN () list is a
// syntax error rather than a match against nothing.
//
// The comparison is over the kind recorded by the most recent failure, so a file
// that failed permanently and then failed for some other reason is retried
// again. That is the safe direction: the newest evidence describes the file now.
func excludeKinds(column string, permanentKinds []string) (string, []any) {
	if len(permanentKinds) == 0 {
		return "", nil
	}
	args := make([]any, len(permanentKinds))
	for i, k := range permanentKinds {
		args[i] = k
	}
	return fmt.Sprintf(" AND %s NOT IN (%s)", column,
		strings.TrimSuffix(strings.Repeat("?,", len(permanentKinds)), ",")), args
}

// ResetStuckRecompressFailures clears the failure counter for segments that hit
// the 3-failure cap without being recompressed, except those whose last failure
// was one of permanentKinds. Called at recorder startup so a transient failure
// (a host that was temporarily missing OpenH264) does not exclude a segment
// forever, while a file that cannot be recompressed at all stays retired instead
// of consuming three attempts on every restart. Returns the number of rows
// reset.
func (d *DB) ResetStuckRecompressFailures(permanentKinds []string) (int64, error) {
	clause, args := excludeKinds("recompress_failure_kind", permanentKinds)
	res, err := d.db.Exec(
		"UPDATE segments SET recompress_failures = 0 WHERE recompress_failures >= 3 AND recompressed = FALSE"+clause,
		args...,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// MarkSegmentRecompressed updates a segment after successful recompression.
func (d *DB) MarkSegmentRecompressed(id int64, newSizeBytes int64) error {
	_, err := d.db.Exec(`
		UPDATE segments
		SET recompressed = TRUE, recompressed_at = ?, size_bytes = ?
		WHERE id = ?`,
		utc(time.Now()), newSizeBytes, id,
	)
	return err
}

// IncrementSegmentRecompressFailures increments the failure counter for a
// segment and records why it failed. Once the counter reaches 3, the segment is
// excluded from future recompression queries; the recorded kind decides whether
// the startup reset frees it again. An empty kind means the cause was not
// identified, which the reset treats as retryable.
func (d *DB) IncrementSegmentRecompressFailures(id int64, kind string) error {
	_, err := d.db.Exec(
		"UPDATE segments SET recompress_failures = recompress_failures + 1, recompress_failure_kind = ? WHERE id = ?",
		kind, id,
	)
	return err
}

// ClipRecord identifies an event clip eligible for recompression.
type ClipRecord struct {
	EventID            string
	Camera             string
	ClipPath           string
	ClipSizeBytes      int64
	EndTime            time.Time
	RecompressFailures int
}

// clipRecompressEligible is the shared WHERE clause selecting clips that can be
// recompressed: a present clip file (clip_available), an event that has ended
// before the cutoff, not yet recompressed, and under the 3-failure cap. The
// single positional parameter is the cutoff.
const clipRecompressEligible = `clip_available = TRUE AND end_time IS NOT NULL AND end_time < ? AND recompressed = FALSE AND recompress_failures < 3`

// GetClipsForRecompression returns eligible clips for a camera ordered oldest
// first (for priority=oldest).
func (d *DB) GetClipsForRecompression(camera string, cutoff time.Time) ([]ClipRecord, error) {
	rows, err := d.db.Query(`
		SELECT id, camera, clip_path, clip_size_bytes, end_time, recompress_failures
		FROM events
		WHERE camera = ? AND `+clipRecompressEligible+`
		ORDER BY end_time ASC`,
		camera, utc(cutoff),
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanClips(rows)
}

// GetClipRecompressionCandidatesBySize returns eligible clips for a camera
// ordered largest first (for priority=largest), capped at limit.
func (d *DB) GetClipRecompressionCandidatesBySize(camera string, cutoff time.Time, limit int) ([]ClipRecord, error) {
	rows, err := d.db.Query(`
		SELECT id, camera, clip_path, clip_size_bytes, end_time, recompress_failures
		FROM events
		WHERE camera = ? AND `+clipRecompressEligible+`
		ORDER BY clip_size_bytes DESC, end_time ASC
		LIMIT ?`,
		camera, utc(cutoff), limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanClips(rows)
}

func scanClips(rows *sql.Rows) ([]ClipRecord, error) {
	var out []ClipRecord
	for rows.Next() {
		var c ClipRecord
		var clipPath sql.NullString
		var endTime sql.NullTime
		if err := rows.Scan(&c.EventID, &c.Camera, &clipPath, &c.ClipSizeBytes, &endTime, &c.RecompressFailures); err != nil {
			return nil, err
		}
		c.ClipPath = clipPath.String
		if endTime.Valid {
			c.EndTime = endTime.Time
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ResetStuckClipRecompressFailures clears the failure counter for clips that
// hit the 3-failure cap without being recompressed, except those whose last
// failure was one of permanentKinds, so a transiently missing codec does not
// exclude a clip forever while an unrecompressable one stays retired. Returns
// the number of rows reset.
func (d *DB) ResetStuckClipRecompressFailures(permanentKinds []string) (int64, error) {
	clause, args := excludeKinds("recompress_failure_kind", permanentKinds)
	res, err := d.db.Exec(
		"UPDATE events SET recompress_failures = 0 WHERE recompress_failures >= 3 AND recompressed = FALSE AND clip_available = TRUE"+clause,
		args...,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// MarkClipRecompressed updates an event after its clip was successfully
// recompressed: it sets the flag, the timestamp, and the new on-disk size.
func (d *DB) MarkClipRecompressed(eventID string, newSize int64) error {
	_, err := d.db.Exec(`
		UPDATE events
		SET recompressed = TRUE, recompressed_at = ?, clip_size_bytes = ?
		WHERE id = ?`,
		utc(time.Now()), newSize, eventID,
	)
	return err
}

// IncrementClipRecompressFailures increments the failure counter for a clip and
// records why it failed. Once the counter reaches 3, the clip is excluded from
// future recompression queries; the recorded kind decides whether the startup
// reset frees it again. An empty kind means the cause was not identified, which
// the reset treats as retryable.
func (d *DB) IncrementClipRecompressFailures(eventID string, kind string) error {
	_, err := d.db.Exec(
		"UPDATE events SET recompress_failures = recompress_failures + 1, recompress_failure_kind = ? WHERE id = ?",
		kind, eventID,
	)
	return err
}

// ClipRecompressState is the minimal row state needed to revalidate a clip
// recompression candidate under the segment-operation lock.
type ClipRecompressState struct {
	ClipPath      string
	ClipAvailable bool
	Recompressed  bool
}

// GetClipRecompressState reads the revalidation fields for one event. The bool
// return reports whether the row exists. GetEventByID returns a camera.Event
// without recompression fields, so a dedicated query is used here.
func (d *DB) GetClipRecompressState(eventID string) (ClipRecompressState, bool, error) {
	var s ClipRecompressState
	var clipPath sql.NullString
	err := d.db.QueryRow(
		"SELECT clip_path, clip_available, recompressed FROM events WHERE id = ?", eventID,
	).Scan(&clipPath, &s.ClipAvailable, &s.Recompressed)
	if err == sql.ErrNoRows {
		return ClipRecompressState{}, false, nil
	}
	if err != nil {
		return ClipRecompressState{}, false, err
	}
	s.ClipPath = clipPath.String
	return s, true, nil
}

// BackfillClipSizes examines up to batch clips that are available but have no
// recorded size (legacy rows at clip_size_bytes = 0) and persists their on-disk
// size. A clip whose file is missing is reconciled to clip_available = 0 (the
// same conclusion reconcileEventMediaAvailability reaches), so every examined
// row leaves the candidate set and a bounded caller loop terminates. Returns
// the number of rows examined.
func (d *DB) BackfillClipSizes(batch int) (int, error) {
	rows, err := d.db.Query(`
		SELECT id, clip_path FROM events
		WHERE clip_available = TRUE AND clip_size_bytes = 0 AND clip_path != ''
		LIMIT ?`, batch)
	if err != nil {
		return 0, err
	}
	type pending struct{ id, path string }
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.path); err != nil {
			_ = rows.Close()
			return 0, err
		}
		todo = append(todo, p)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	_ = rows.Close()

	for _, p := range todo {
		fi, statErr := os.Stat(p.path)
		if statErr != nil {
			if _, err := d.db.Exec("UPDATE events SET clip_available = FALSE WHERE id = ?", p.id); err != nil {
				return len(todo), err
			}
			continue
		}
		if _, err := d.db.Exec("UPDATE events SET clip_size_bytes = ? WHERE id = ?", fi.Size(), p.id); err != nil {
			return len(todo), err
		}
	}
	return len(todo), nil
}

// GetRecordingDays returns sorted day numbers (in loc) that have recording
// coverage for the given camera and month. The month is defined by year/month
// interpreted in loc, so days line up with the calendar the user sees. A day
// counts when any segment overlaps it, including segments spanning midnight.
// If camera is empty, days across all cameras are returned.
func (d *DB) GetRecordingDays(camera string, year int, month int, loc *time.Location) ([]int, error) {
	if loc == nil {
		loc = time.UTC
	}
	monthStart := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, loc)
	monthEnd := monthStart.AddDate(0, 1, 0)

	segments, err := d.GetSegmentsOverlapping(camera, monthStart, monthEnd)
	if err != nil {
		return nil, err
	}

	covered := make(map[int]bool)
	for _, seg := range segments {
		start := seg.StartTime
		if start.Before(monthStart) {
			start = monthStart
		}
		end := seg.EndTime
		if end.After(monthEnd) {
			end = monthEnd
		}
		// Mark every local day the segment touches. Day-by-day via AddDate
		// stays correct across DST transitions (23/25-hour days).
		day := time.Date(start.In(loc).Year(), start.In(loc).Month(), start.In(loc).Day(), 0, 0, 0, 0, loc)
		for day.Before(end) {
			if !day.Before(monthStart) {
				covered[day.Day()] = true
			}
			day = day.AddDate(0, 0, 1)
		}
	}

	days := make([]int, 0, len(covered))
	for day := range covered {
		days = append(days, day)
	}
	sort.Ints(days)
	return days, nil
}

// GetAdjacentEvents returns the previous and next event IDs relative to the given event,
// ordered by timestamp.
func (d *DB) GetAdjacentEvents(id string) (prevID, nextID string, err error) {
	return d.GetAdjacentEventsFiltered(id, EventFilters{})
}

// GetAdjacentEventsFiltered returns the previous and next event IDs relative
// to the given event while preserving the active event-review filters.
func (d *DB) GetAdjacentEventsFiltered(id string, filters EventFilters) (prevID, nextID string, err error) {
	where, args := eventFilterClause(filters)
	prevArgs := append(append([]any{}, args...), id)
	err = d.db.QueryRow(`
		SELECT id FROM events`+where+`
		AND timestamp < (SELECT timestamp FROM events WHERE id = ?)
		ORDER BY timestamp DESC LIMIT 1`, prevArgs...).Scan(&prevID)
	if err == sql.ErrNoRows {
		prevID = ""
		err = nil
	}
	if err != nil {
		return "", "", err
	}

	nextArgs := append(append([]any{}, args...), id)
	err = d.db.QueryRow(`
		SELECT id FROM events`+where+`
		AND timestamp > (SELECT timestamp FROM events WHERE id = ?)
		ORDER BY timestamp ASC LIMIT 1`, nextArgs...).Scan(&nextID)
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
		var box [4]sql.NullInt64
		var endTime sql.NullTime
		var snapshot, clip, zoneName, objectName, subLabel sql.NullString
		var category string
		var snapshotAvailable, clipAvailable bool
		var kind string
		var answeredAt sql.NullTime
		var answeredBy sql.NullString
		err := rows.Scan(&e.ID, &e.CameraName, &e.Label, &e.Score,
			&box[0], &box[1], &box[2], &box[3],
			&e.Timestamp, &endTime, &snapshot, &snapshotAvailable, &clip, &clipAvailable, &zoneName, &objectName, &subLabel, &category,
			&kind, &answeredAt, &answeredBy,
		)
		if err != nil {
			return nil, err
		}
		for i := range box {
			if box[i].Valid {
				e.Box[i] = int(box[i].Int64)
			}
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
		e.Category = category
		e.Kind = kind
		if answeredAt.Valid {
			e.AnsweredAt = answeredAt.Time
		}
		e.AnsweredBy = answeredBy.String
		events = append(events, e)
	}
	return events, rows.Err()
}

// DeleteEvent removes an event by ID.
func (d *DB) DeleteEvent(id string) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var activityID string
	err = tx.QueryRow(`SELECT activity_id FROM activity_events WHERE event_id = ?`, id).Scan(&activityID)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if _, err := tx.Exec("DELETE FROM events WHERE id = ?", id); err != nil {
		return err
	}
	if activityID != "" {
		if _, err := reconcileActivityTx(tx, activityID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// EventMediaRef is the minimal projection of an event needed to reconcile its
// recorded media against the filesystem. It deliberately omits the heavy event
// fields so reconciliation does not hydrate full event rows.
type EventMediaRef struct {
	ID                string
	SnapshotPath      string
	SnapshotAvailable bool
	ClipPath          string
	ClipAvailable     bool
}

// EventMediaRefs returns the media references for every event that has a
// non-empty snapshot or clip path. Used by media-availability reconciliation.
func (d *DB) EventMediaRefs() ([]EventMediaRef, error) {
	rows, err := d.db.Query(`
		SELECT id, snapshot_path, snapshot_available, clip_path, clip_available
		FROM events
		WHERE (snapshot_path != '' AND snapshot_path IS NOT NULL)
		   OR (clip_path != '' AND clip_path IS NOT NULL)`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var refs []EventMediaRef
	for rows.Next() {
		var (
			ref      EventMediaRef
			snapPath sql.NullString
			clipPath sql.NullString
		)
		if err := rows.Scan(&ref.ID, &snapPath, &ref.SnapshotAvailable, &clipPath, &ref.ClipAvailable); err != nil {
			return nil, err
		}
		ref.SnapshotPath = snapPath.String
		ref.ClipPath = clipPath.String
		refs = append(refs, ref)
	}
	return refs, rows.Err()
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
		var recompressedAt sql.NullTime
		if err := rows.Scan(
			&seg.ID, &seg.Camera, &seg.Path,
			&seg.StartTime, &seg.EndTime, &seg.SizeBytes,
			&seg.Recompressed, &recompressedAt, &seg.RecompressFailures,
		); err != nil {
			return nil, err
		}
		if recompressedAt.Valid {
			seg.RecompressedAt = recompressedAt.Time
		}
		segments = append(segments, seg)
	}
	return segments, rows.Err()
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

// SegmentBytesSince returns the total size_bytes of segments whose start_time
// is newer than the given cutoff. Used for computing recent ingest rate.
func (d *DB) SegmentBytesSince(cutoff time.Time) (int64, error) {
	var bytes sql.NullInt64
	err := d.db.QueryRow(
		"SELECT COALESCE(SUM(size_bytes), 0) FROM segments WHERE start_time > ?",
		utc(cutoff),
	).Scan(&bytes)
	if err != nil {
		return 0, err
	}
	return bytes.Int64, nil
}

// OldestSegmentTime returns the start_time of the oldest segment, or the zero
// time if there are no segments.
func (d *DB) OldestSegmentTime() (time.Time, error) {
	var oldest sql.NullString
	err := d.db.QueryRow("SELECT MIN(start_time) FROM segments").Scan(&oldest)
	if err != nil {
		return time.Time{}, err
	}
	if !oldest.Valid {
		return time.Time{}, nil
	}
	return parseStoredTime(oldest.String)
}

// GetSetting retrieves the value for a key from the kv_store.
// Returns an empty string (no error) when the key does not exist.
func (d *DB) GetSetting(key string) (string, error) {
	var value string
	err := d.db.QueryRow("SELECT value FROM kv_store WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

// SetSetting stores or updates a key-value pair in the kv_store.
func (d *DB) SetSetting(key, value string) error {
	_, err := d.db.Exec(
		"INSERT INTO kv_store (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		key, value,
	)
	return err
}

// DeleteSetting removes a key from the kv_store. Deleting a non-existent key is not an error.
func (d *DB) DeleteSetting(key string) error {
	_, err := d.db.Exec("DELETE FROM kv_store WHERE key = ?", key)
	return err
}

// GetKV reads a kv_store row, distinguishing missing keys from empty values.
// Wrapper that makes *DB satisfy notify.KVStore without forcing callers to
// reason about GetSetting's "empty string on missing" contract.
func (d *DB) GetKV(key string) (string, bool, error) {
	var val string
	err := d.db.QueryRow("SELECT value FROM kv_store WHERE key = ?", key).Scan(&val)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return val, true, nil
}

// SetKV upserts a kv_store row. Equivalent to SetSetting; exists so that
// *DB satisfies notify.KVStore with symmetric naming.
func (d *DB) SetKV(key, value string) error {
	return d.SetSetting(key, value)
}

// SetCameraStopped marks a camera as stopped (true) or running (false) in the kv_store.
func (d *DB) SetCameraStopped(name string, stopped bool) error {
	key := "camera_stopped:" + name
	if stopped {
		return d.SetSetting(key, "1")
	}
	return d.DeleteSetting(key)
}

// ListStoppedCameras returns the names of all cameras currently marked as stopped.
func (d *DB) ListStoppedCameras() ([]string, error) {
	rows, err := d.db.Query("SELECT key FROM kv_store WHERE key LIKE 'camera_stopped:%'")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		names = append(names, strings.TrimPrefix(key, "camera_stopped:"))
	}
	return names, rows.Err()
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

// Raw returns the underlying *sql.DB for tests that need to seed or inspect rows directly.
// Production code should not use this.
func (d *DB) Raw() *sql.DB {
	return d.db
}

// ListAllUsernames returns every username that has at least one push
// subscription. It is the source of truth for the notification dispatcher's
// per-event fanout loop.
//
// Deliberately NOT "SELECT username FROM auth_users": push subscriptions
// can belong to users authenticated by any source (direct session, reverse-
// proxy Remote-User, bearer token), and those usernames do not all appear
// in auth_users. Iterating push_subscriptions directly also avoids doing
// pref/mute/cooldown work for users who aren't subscribed.

// eventSelectCols is the fixed column list matched by scanEvents.
const eventSelectCols = "id, camera, label, score, box_x1, box_y1, box_x2, box_y2, timestamp, end_time, snapshot_path, snapshot_available, clip_path, clip_available, zone_name, object_name, sub_label, category, kind, answered_at, answered_by"

// clipPredicate matches events that have at least one media file attached.
const clipPredicate = "(clip_path != '' OR snapshot_path != '')"

// ClipsByCameraInRange returns events for cameraName that have media (clip or
// snapshot) and whose effective time — end_time if set, timestamp otherwise —
// falls within [from, to).
func (d *DB) ClipsByCameraInRange(cameraName string, from, to time.Time) ([]camera.Event, error) {
	rows, err := d.db.Query(`
		SELECT `+eventSelectCols+`
		FROM events
		WHERE camera = ?
		  AND `+clipPredicate+`
		  AND COALESCE(end_time, timestamp) >= ?
		  AND COALESCE(end_time, timestamp) <  ?
		ORDER BY timestamp ASC`,
		cameraName, utc(from), utc(to))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanEvents(rows)
}

// ClipsByCameraOlderThan returns events for cameraName that have media and
// whose effective time — end_time if set, timestamp otherwise — is before
// cutoff.
func (d *DB) ClipsByCameraOlderThan(cameraName string, cutoff time.Time) ([]camera.Event, error) {
	rows, err := d.db.Query(`
		SELECT `+eventSelectCols+`
		FROM events
		WHERE camera = ?
		  AND `+clipPredicate+`
		  AND COALESCE(end_time, timestamp) < ?
		ORDER BY timestamp ASC`,
		cameraName, utc(cutoff))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanEvents(rows)
}

// ClipsByCamera returns all events for cameraName that have media (clip or
// snapshot), regardless of time.
func (d *DB) ClipsByCamera(cameraName string) ([]camera.Event, error) {
	rows, err := d.db.Query(`
		SELECT `+eventSelectCols+`
		FROM events
		WHERE camera = ?
		  AND `+clipPredicate+`
		ORDER BY timestamp ASC`, cameraName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanEvents(rows)
}

func (d *DB) ListAllUsernames() ([]string, error) {
	rows, err := d.db.Query(`SELECT DISTINCT username FROM push_subscriptions ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// StorageAuditEntry is one row of the storage_audit table.
type StorageAuditEntry struct {
	ID        int64
	Timestamp time.Time
	Actor     string
	ScopeJSON string
	Bytes     int64
	Files     int
}

// DayBytes is one row of the per-day segment-bytes aggregation.
type DayBytes struct {
	Date  string
	Bytes int64
}

// PerDayCameraSegmentBytes returns segment-byte totals per day for the
// last N days, ordered ascending by date.
func (d *DB) PerDayCameraSegmentBytes(camera string, days int) ([]DayBytes, error) {
	if days <= 0 {
		days = 30
	}
	// The modernc.org/sqlite driver serializes time.Time via Go's String()
	// ("2006-01-02 15:04:05.999999999 +0000 UTC"), which SQLite's date
	// functions cannot parse. Derive the day from the fixed-width leading
	// "YYYY-MM-DD" instead of strftime/datetime, and compare start_time
	// directly against the canonical (utc-bound) cutoff.
	cutoff := utc(time.Now().AddDate(0, 0, -days))
	rows, err := d.db.Query(`
		SELECT substr(start_time, 1, 10) AS day,
		       COALESCE(SUM(size_bytes), 0) AS bytes
		FROM segments
		WHERE camera = ?
		  AND start_time >= ?
		GROUP BY day
		ORDER BY day ASC`,
		camera, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DayBytes
	for rows.Next() {
		var db DayBytes
		if err := rows.Scan(&db.Date, &db.Bytes); err != nil {
			return nil, err
		}
		out = append(out, db)
	}
	return out, rows.Err()
}

// AllSnapshotPaths returns every non-empty events.snapshot_path.
func (d *DB) AllSnapshotPaths() ([]string, error) {
	rows, err := d.db.Query(`SELECT snapshot_path FROM events WHERE snapshot_path != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// InsertStorageAudit records a manual storage operation.
func (d *DB) InsertStorageAudit(e StorageAuditEntry) error {
	_, err := d.db.Exec(`
		INSERT INTO storage_audit (ts, actor, scope_json, bytes_freed, file_count)
		VALUES (?, ?, ?, ?, ?)`,
		utc(e.Timestamp), e.Actor, e.ScopeJSON, e.Bytes, e.Files)
	return err
}

// StorageAudit returns the most recent audit entries, newest first.
func (d *DB) StorageAudit(limit int) ([]StorageAuditEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.db.Query(`
		SELECT id, ts, actor, scope_json, bytes_freed, file_count
		FROM storage_audit
		ORDER BY ts DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StorageAuditEntry
	for rows.Next() {
		var e StorageAuditEntry
		if err := rows.Scan(&e.ID, &e.Timestamp, &e.Actor, &e.ScopeJSON, &e.Bytes, &e.Files); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
