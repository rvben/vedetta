package storage

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rvben/vedetta/internal/camera"
)

// activityMergeGap is the quiet period that closes a camera-local incident.
// A later event inside this window is additional evidence for the same
// activity. Keeping the rule small and deterministic makes backfill, replay,
// and live ingestion agree.
const activityMergeGap = 90 * time.Second

// Activity is the review-level incident assembled from one or more raw events.
// Events is populated for detail queries; list queries expose PrimaryEvent and
// compact summaries so clients do not need to understand aggregation internals.
type Activity struct {
	ID              string         `json:"id"`
	CameraName      string         `json:"camera"`
	StartTime       time.Time      `json:"start_time"`
	EndTime         time.Time      `json:"end_time"`
	Category        string         `json:"category"`
	EventCount      int            `json:"event_count"`
	DurationSeconds int64          `json:"duration_seconds"`
	Labels          []string       `json:"labels"`
	Zones           []string       `json:"zones"`
	RecognizedNames []string       `json:"recognized_names"`
	HasDoorbell     bool           `json:"has_doorbell"`
	MissedDoorbell  bool           `json:"missed_doorbell"`
	PrimaryEvent    camera.Event   `json:"primary_event"`
	Events          []camera.Event `json:"events,omitempty"`
}

// ActivityFilters narrows review incidents. Evidence filters match an activity
// when any linked event satisfies every supplied evidence predicate.
type ActivityFilters struct {
	Camera   string
	Label    string
	Zone     string
	Object   string
	Category string
	Kind     string
	Search   string
	After    time.Time
	Before   time.Time
}

type activityRow struct {
	ID         string
	CameraName string
	StartTime  time.Time
	EndTime    time.Time
	Category   string
}

func eventActivityBounds(event camera.Event) (time.Time, time.Time) {
	start := utc(event.Timestamp)
	end := start
	if !event.EndTime.IsZero() && event.EndTime.After(event.Timestamp) {
		end = utc(event.EndTime)
	}
	return start, end
}

// assignEventToActivityTx links an inserted event and merges every episode it
// bridges. The earliest activity remains canonical, so chronological backfill
// produces stable IDs while late evidence remains safe and idempotent.
func assignEventToActivityTx(tx *sql.Tx, event camera.Event) error {
	start, end := eventActivityBounds(event)
	rows, err := tx.Query(`
		SELECT id, start_time
		FROM activities
		WHERE camera = ?
		  AND start_time <= ?
		  AND end_time >= ?
		ORDER BY start_time ASC, id ASC`,
		event.CameraName, utc(end.Add(activityMergeGap)), utc(start.Add(-activityMergeGap)))
	if err != nil {
		return fmt.Errorf("find neighboring activities: %w", err)
	}

	var candidates []activityRow
	for rows.Next() {
		var row activityRow
		if err := rows.Scan(&row.ID, &row.StartTime); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan neighboring activity: %w", err)
		}
		candidates = append(candidates, row)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close neighboring activities: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate neighboring activities: %w", err)
	}

	activityID := "act_" + event.ID
	if len(candidates) == 0 {
		category := event.Category
		if category == "" {
			category = camera.CategoryAlert
		}
		if _, err := tx.Exec(`
			INSERT INTO activities (id, camera, start_time, end_time, category, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			activityID, event.CameraName, start, end, category, utc(time.Now())); err != nil {
			return fmt.Errorf("create activity: %w", err)
		}
	} else {
		activityID = candidates[0].ID
		for _, candidate := range candidates[1:] {
			if _, err := tx.Exec(`UPDATE activity_events SET activity_id = ? WHERE activity_id = ?`, activityID, candidate.ID); err != nil {
				return fmt.Errorf("merge activity evidence: %w", err)
			}
			if _, err := tx.Exec(`DELETE FROM activities WHERE id = ?`, candidate.ID); err != nil {
				return fmt.Errorf("delete merged activity: %w", err)
			}
		}
	}

	if _, err := tx.Exec(`
		INSERT INTO activity_events (activity_id, event_id)
		VALUES (?, ?)
		ON CONFLICT(event_id) DO UPDATE SET activity_id = excluded.activity_id`, activityID, event.ID); err != nil {
		return fmt.Errorf("link event to activity: %w", err)
	}
	_, err = reconcileActivityTx(tx, activityID)
	return err
}

// reconcileActivityTx recomputes durable bounds/category and merges neighbors
// exposed by an event-end update. It returns the surviving activity ID, or an
// empty string when the activity no longer has evidence.
func reconcileActivityTx(tx *sql.Tx, activityID string) (string, error) {
	for activityID != "" {
		var evidenceCount int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM activity_events WHERE activity_id = ?`, activityID).Scan(&evidenceCount); err != nil {
			return "", fmt.Errorf("count activity evidence: %w", err)
		}
		if evidenceCount == 0 {
			if _, err := tx.Exec(`DELETE FROM activities WHERE id = ?`, activityID); err != nil {
				return "", fmt.Errorf("delete empty activity: %w", err)
			}
			return "", nil
		}
		var (
			cameraName string
			startRaw   string
			endRaw     string
			category   string
		)
		err := tx.QueryRow(`
			SELECT MIN(e.camera), MIN(e.timestamp),
			       MAX(COALESCE(e.end_time, e.timestamp)),
			       CASE WHEN SUM(CASE WHEN e.category = 'alert' THEN 1 ELSE 0 END) > 0
			            THEN 'alert' ELSE 'detection' END
			FROM activity_events ae
			JOIN events e ON e.id = ae.event_id
			WHERE ae.activity_id = ?`, activityID).Scan(&cameraName, &startRaw, &endRaw, &category)
		if err != nil {
			return "", fmt.Errorf("summarize activity: %w", err)
		}
		start, err := parseStoredTime(startRaw)
		if err != nil {
			return "", fmt.Errorf("parse activity start: %w", err)
		}
		end, err := parseStoredTime(endRaw)
		if err != nil {
			return "", fmt.Errorf("parse activity end: %w", err)
		}
		if _, err := tx.Exec(`
			UPDATE activities
			SET camera = ?, start_time = ?, end_time = ?, category = ?, updated_at = ?
			WHERE id = ?`, cameraName, utc(start), utc(end), category, utc(time.Now()), activityID); err != nil {
			return "", fmt.Errorf("update activity summary: %w", err)
		}

		rows, err := tx.Query(`
			SELECT id, start_time
			FROM activities
			WHERE camera = ? AND id != ?
			  AND start_time <= ? AND end_time >= ?
			ORDER BY start_time ASC, id ASC`,
			cameraName, activityID, utc(end.Add(activityMergeGap)), utc(start.Add(-activityMergeGap)))
		if err != nil {
			return "", fmt.Errorf("find activities after reconcile: %w", err)
		}
		var neighbors []activityRow
		for rows.Next() {
			var neighbor activityRow
			if err := rows.Scan(&neighbor.ID, &neighbor.StartTime); err != nil {
				_ = rows.Close()
				return "", fmt.Errorf("scan activity after reconcile: %w", err)
			}
			neighbors = append(neighbors, neighbor)
		}
		if err := rows.Close(); err != nil {
			return "", fmt.Errorf("close activities after reconcile: %w", err)
		}
		if len(neighbors) == 0 {
			return activityID, nil
		}

		canonicalID := activityID
		canonicalStart := start
		for _, neighbor := range neighbors {
			if neighbor.StartTime.Before(canonicalStart) || (neighbor.StartTime.Equal(canonicalStart) && neighbor.ID < canonicalID) {
				canonicalID = neighbor.ID
				canonicalStart = neighbor.StartTime
			}
		}
		mergeIDs := []string{activityID}
		for _, neighbor := range neighbors {
			mergeIDs = append(mergeIDs, neighbor.ID)
		}
		for _, mergeID := range mergeIDs {
			if mergeID == canonicalID {
				continue
			}
			if _, err := tx.Exec(`UPDATE activity_events SET activity_id = ? WHERE activity_id = ?`, canonicalID, mergeID); err != nil {
				return "", fmt.Errorf("merge reconciled evidence: %w", err)
			}
			if _, err := tx.Exec(`DELETE FROM activities WHERE id = ?`, mergeID); err != nil {
				return "", fmt.Errorf("delete reconciled activity: %w", err)
			}
		}
		activityID = canonicalID
	}
	return "", nil
}

func backfillActivities(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.Query(`SELECT ` + eventSelectCols + ` FROM events ORDER BY timestamp ASC, id ASC`)
	if err != nil {
		return err
	}
	events, err := scanEvents(rows)
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, event := range events {
		if err := assignEventToActivityTx(tx, event); err != nil {
			return fmt.Errorf("event %s: %w", event.ID, err)
		}
	}
	return tx.Commit()
}

func activityFilterClause(filters ActivityFilters) (string, []any) {
	clauses := []string{"1=1"}
	args := []any{}
	if filters.Camera != "" {
		clauses = append(clauses, "a.camera = ?")
		args = append(args, filters.Camera)
	}
	if !filters.After.IsZero() {
		clauses = append(clauses, "a.end_time >= ?")
		args = append(args, utc(filters.After))
	}
	if !filters.Before.IsZero() {
		clauses = append(clauses, "a.start_time < ?")
		args = append(args, utc(filters.Before))
	}

	evidence := []string{"ae.activity_id = a.id"}
	evidenceArgs := []any{}
	if filters.Label != "" {
		evidence = append(evidence, "e.label = ?")
		evidenceArgs = append(evidenceArgs, filters.Label)
	}
	if filters.Zone != "" {
		evidence = append(evidence, "e.zone_name = ?")
		evidenceArgs = append(evidenceArgs, filters.Zone)
	}
	if filters.Object != "" {
		evidence = append(evidence, "(e.object_name = ? OR e.sub_label = ?)")
		evidenceArgs = append(evidenceArgs, filters.Object, filters.Object)
	}
	if filters.Category != "" {
		evidence = append(evidence, "e.category = ?")
		evidenceArgs = append(evidenceArgs, filters.Category)
	}
	if filters.Kind != "" {
		evidence = append(evidence, "e.kind = ?")
		evidenceArgs = append(evidenceArgs, filters.Kind)
	}
	if query := strings.TrimSpace(filters.Search); query != "" {
		like := "%" + query + "%"
		evidence = append(evidence, "(e.camera LIKE ? OR e.label LIKE ? OR IFNULL(e.zone_name,'') LIKE ? OR IFNULL(e.object_name,'') LIKE ? OR IFNULL(e.sub_label,'') LIKE ?)")
		evidenceArgs = append(evidenceArgs, like, like, like, like, like)
	}
	if len(evidence) > 1 {
		clauses = append(clauses, "EXISTS (SELECT 1 FROM activity_events ae JOIN events e ON e.id = ae.event_id WHERE "+strings.Join(evidence, " AND ")+")")
		args = append(args, evidenceArgs...)
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// QueryActivitiesFiltered returns incident-sized review items in newest-first
// order. Primary evidence and summary facets are derived from current event
// rows, so late snapshots and recognition updates appear without rewriting the
// activity record.
func (d *DB) QueryActivitiesFiltered(filters ActivityFilters, limit, offset int) ([]Activity, error) {
	where, args := activityFilterClause(filters)
	query := `SELECT a.id, a.camera, a.start_time, a.end_time, a.category FROM activities a` + where + ` ORDER BY a.start_time DESC, a.id DESC`
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

	var activities []Activity
	for rows.Next() {
		var activity Activity
		if err := rows.Scan(&activity.ID, &activity.CameraName, &activity.StartTime, &activity.EndTime, &activity.Category); err != nil {
			_ = rows.Close()
			return nil, err
		}
		activities = append(activities, activity)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	// Finish the base query before loading evidence. Besides avoiding a held
	// cursor during nested reads, this keeps the method safe when SQLite is
	// configured with a single available connection.
	for i := range activities {
		if err := d.hydrateActivity(&activities[i], false); err != nil {
			return nil, err
		}
	}
	return activities, nil
}

func (d *DB) CountActivitiesFiltered(filters ActivityFilters) (int, error) {
	where, args := activityFilterClause(filters)
	var count int
	err := d.db.QueryRow(`SELECT COUNT(*) FROM activities a`+where, args...).Scan(&count)
	return count, err
}

func (d *DB) GetActivityByID(id string) (*Activity, error) {
	var activity Activity
	err := d.db.QueryRow(`
		SELECT id, camera, start_time, end_time, category
		FROM activities WHERE id = ?`, id).Scan(
		&activity.ID, &activity.CameraName, &activity.StartTime, &activity.EndTime, &activity.Category)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := d.hydrateActivity(&activity, true); err != nil {
		return nil, err
	}
	return &activity, nil
}

func (d *DB) hydrateActivity(activity *Activity, includeEvents bool) error {
	rows, err := d.db.Query(`
		SELECT `+eventSelectCols+`
		FROM events e
		JOIN activity_events ae ON ae.event_id = e.id
		WHERE ae.activity_id = ?
		ORDER BY e.timestamp ASC, e.id ASC`, activity.ID)
	if err != nil {
		return err
	}
	events, err := scanEvents(rows)
	_ = rows.Close()
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return fmt.Errorf("activity %s has no evidence", activity.ID)
	}

	activity.EventCount = len(events)
	activity.DurationSeconds = max(int64(activity.EndTime.Sub(activity.StartTime).Seconds()), 0)
	labels := map[string]struct{}{}
	zones := map[string]struct{}{}
	names := map[string]struct{}{}
	best := -1
	bestScore := -1.0
	for i, event := range events {
		if event.Label != "" {
			labels[event.Label] = struct{}{}
		}
		if event.ZoneName != "" {
			zones[event.ZoneName] = struct{}{}
		}
		for _, name := range []string{event.SubLabel, event.ObjectName} {
			if name != "" && name != "_ignored" {
				names[name] = struct{}{}
			}
		}
		if event.Kind == camera.EventKindDoorbell {
			activity.HasDoorbell = true
			if event.AnsweredAt.IsZero() {
				activity.MissedDoorbell = true
			}
		}
		rank := float64(event.Score)
		if event.SnapshotAvailable {
			rank += 1000
		}
		if event.Category != camera.CategoryDetection {
			rank += 100
		}
		if event.SubLabel != "" || event.ObjectName != "" {
			rank += 50
		}
		if rank > bestScore {
			best = i
			bestScore = rank
		}
	}
	activity.Labels = sortedKeys(labels)
	activity.Zones = sortedKeys(zones)
	activity.RecognizedNames = sortedKeys(names)
	activity.PrimaryEvent = events[best]
	if includeEvents {
		activity.Events = events
	}
	return nil
}

func sortedKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
