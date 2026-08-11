package storage

import (
	"database/sql"
	"time"

	"github.com/rvben/vedetta/internal/camera"
)

// KnownObject represents a user-defined object to recognize (e.g. "Ruben's car").
type KnownObject struct {
	ID             int64     `json:"id"`
	Name           string    `json:"name"`
	Label          string    `json:"label"`
	Centroid       []byte    `json:"-"`
	CropPath       string    `json:"crop_path,omitempty"`
	MatchThreshold *float64  `json:"match_threshold,omitempty"`
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
			snapshot_path, snapshot_available, clip_path, clip_available, zone_name, object_name, sub_label, category, kind, answered_at, answered_by
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

// SaveObjectRecognition atomically records a sighting and applies the known
// object's name to the event. Recognition callers should use this instead of
// issuing independent writes that can leave the sighting and event labels out
// of sync after a partial failure.
func (d *DB) SaveObjectRecognition(s ObjectSighting) (int64, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.Exec(`
		INSERT INTO object_sightings (event_id, camera, object_id, similarity, timestamp)
		VALUES (?, ?, ?, ?, ?)`,
		s.EventID, s.Camera, s.ObjectID, s.Similarity, utc(s.Timestamp),
	)
	if err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(
		"UPDATE events SET object_name = ?, sub_label = ? WHERE id = ?",
		s.ObjectName, s.ObjectName, s.EventID,
	); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
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
