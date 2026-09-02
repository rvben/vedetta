package storage

import (
	"database/sql"
	"errors"
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

var (
	ErrActivityNotFound        = errors.New("activity not found")
	ErrActivityEvidenceMissing = errors.New("activity evidence not found")
	ErrActivityNeedsEvidence   = errors.New("an activity must retain at least one evidence event")
)

type ActivityState string

const (
	ActivityStateOpen      ActivityState = "open"
	ActivityStateFinalized ActivityState = "finalized"
)

// ActivityGrouping describes the deterministic rule used to assemble raw
// detections into a review-level incident.
type ActivityGrouping struct {
	Strategy           string `json:"strategy"`
	QuietPeriodSeconds int64  `json:"quiet_period_seconds"`
	Explanation        string `json:"explanation"`
}

// ActivityEvidenceCorrection is an active operator decision to keep a raw
// event visible but out of the incident summary. Restored decisions remain in
// the database as history and are omitted from this active view.
type ActivityEvidenceCorrection struct {
	Event     camera.Event `json:"event"`
	Action    string       `json:"action"`
	Reason    string       `json:"reason,omitempty"`
	Actor     string       `json:"actor"`
	CreatedAt time.Time    `json:"created_at"`
}

// Activity is the review-level incident assembled from one or more raw events.
// Events is populated for detail queries; list queries expose PrimaryEvent and
// compact summaries so clients do not need to understand aggregation internals.
type Activity struct {
	ID               string                       `json:"id"`
	CameraName       string                       `json:"camera"`
	StartTime        time.Time                    `json:"start_time"`
	EndTime          time.Time                    `json:"end_time"`
	Category         string                       `json:"category"`
	State            ActivityState                `json:"state"`
	ClosesAt         time.Time                    `json:"closes_at"`
	FinalizedAt      *time.Time                   `json:"finalized_at,omitempty"`
	UpdatedAt        time.Time                    `json:"updated_at"`
	EventCount       int                          `json:"event_count"`
	DurationSeconds  int64                        `json:"duration_seconds"`
	Labels           []string                     `json:"labels"`
	Zones            []string                     `json:"zones"`
	RecognizedNames  []string                     `json:"recognized_names"`
	HasDoorbell      bool                         `json:"has_doorbell"`
	MissedDoorbell   bool                         `json:"missed_doorbell"`
	Grouping         ActivityGrouping             `json:"grouping"`
	PrimaryEvent     camera.Event                 `json:"primary_event"`
	Events           []camera.Event               `json:"events,omitempty"`
	ExcludedEvidence []ActivityEvidenceCorrection `json:"excluded_evidence,omitempty"`
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
	State    ActivityState
	Search   string
	After    time.Time
	Before   time.Time
}

type activityRow struct {
	ID        string
	StartTime time.Time
}

const activitySelectCols = "a.id, a.camera, a.start_time, a.end_time, a.category, a.state, a.finalized_at, a.updated_at"

type rowScanner interface {
	Scan(dest ...any) error
}

func scanActivityBase(scanner rowScanner, activity *Activity) error {
	var finalizedAt sql.NullTime
	if err := scanner.Scan(&activity.ID, &activity.CameraName, &activity.StartTime, &activity.EndTime,
		&activity.Category, &activity.State, &finalizedAt, &activity.UpdatedAt); err != nil {
		return err
	}
	activity.ClosesAt = activity.EndTime.Add(activityMergeGap)
	activity.Grouping = ActivityGrouping{
		Strategy:           "camera_local_quiet_period",
		QuietPeriodSeconds: int64(activityMergeGap / time.Second),
		Explanation:        "Evidence from the same camera is grouped while detections remain no more than 90 seconds apart.",
	}
	if finalizedAt.Valid {
		finalized := finalizedAt.Time
		activity.FinalizedAt = &finalized
	}
	return nil
}

func moveActivityCorrectionsTx(tx *sql.Tx, canonicalID, mergedID string) error {
	if _, err := tx.Exec(`UPDATE activity_event_corrections SET activity_id = ? WHERE activity_id = ?`, canonicalID, mergedID); err != nil {
		return fmt.Errorf("merge activity correction history: %w", err)
	}
	return nil
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
			INSERT INTO activities (id, camera, start_time, end_time, category, state, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			activityID, event.CameraName, start, end, category, ActivityStateOpen, utc(time.Now())); err != nil {
			return fmt.Errorf("create activity: %w", err)
		}
	} else {
		activityID = candidates[0].ID
		for _, candidate := range candidates[1:] {
			if err := preserveActivityNotificationTx(tx, activityID, candidate.ID); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE activity_events SET activity_id = ? WHERE activity_id = ?`, activityID, candidate.ID); err != nil {
				return fmt.Errorf("merge activity evidence: %w", err)
			}
			if err := moveActivityCorrectionsTx(tx, activityID, candidate.ID); err != nil {
				return err
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
	activityID, err = reconcileActivityTx(tx, activityID)
	if err != nil || activityID == "" {
		return err
	}
	return reopenActivityTx(tx, activityID)
}

func preserveActivityNotificationTx(tx *sql.Tx, canonicalID, mergedID string) error {
	_, err := tx.Exec(`
		UPDATE activities
		SET notification_queued_at = COALESCE(
			notification_queued_at,
			(SELECT notification_queued_at FROM activities WHERE id = ?))
		WHERE id = ?`, mergedID, canonicalID)
	if err != nil {
		return fmt.Errorf("preserve activity notification state: %w", err)
	}
	return nil
}

func reopenActivityTx(tx *sql.Tx, activityID string) error {
	_, err := tx.Exec(`
		UPDATE activities
		SET state = ?, finalized_at = NULL, updated_at = ?
		WHERE id = ?`, ActivityStateOpen, utc(time.Now()), activityID)
	if err != nil {
		return fmt.Errorf("reopen activity: %w", err)
	}
	return nil
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
			if err := preserveActivityNotificationTx(tx, canonicalID, mergeID); err != nil {
				return "", err
			}
			if _, err := tx.Exec(`UPDATE activity_events SET activity_id = ? WHERE activity_id = ?`, canonicalID, mergeID); err != nil {
				return "", fmt.Errorf("merge reconciled evidence: %w", err)
			}
			if err := moveActivityCorrectionsTx(tx, canonicalID, mergeID); err != nil {
				return "", err
			}
			if _, err := tx.Exec(`DELETE FROM activities WHERE id = ?`, mergeID); err != nil {
				return "", fmt.Errorf("delete reconciled activity: %w", err)
			}
		}
		activityID = canonicalID
	}
	return "", nil
}

// backfillActivities groups every existing event into an activity. It runs
// inside the caller's transaction so the backfill and the schema version that
// records it commit together.
func backfillActivities(tx *sql.Tx) error {
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
	return nil
}

func activityFilterClause(filters ActivityFilters) (string, []any) {
	clauses := []string{"1=1"}
	args := []any{}
	if filters.Camera != "" {
		clauses = append(clauses, "a.camera = ?")
		args = append(args, filters.Camera)
	}
	if filters.State != "" {
		clauses = append(clauses, "a.state = ?")
		args = append(args, filters.State)
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
	query := `SELECT ` + activitySelectCols + ` FROM activities a` + where + ` ORDER BY a.start_time DESC, a.id DESC`
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
		if err := scanActivityBase(rows, &activity); err != nil {
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
	row := d.db.QueryRow(`
		SELECT `+activitySelectCols+`
		FROM activities a WHERE a.id = ?`, id)
	err := scanActivityBase(row, &activity)
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

func (d *DB) GetActivityByEventID(eventID string) (*Activity, error) {
	var activity Activity
	row := d.db.QueryRow(`
		SELECT `+activitySelectCols+`
		FROM activities a
		JOIN activity_events ae ON ae.activity_id = a.id
		WHERE ae.event_id = ?`, eventID)
	if err := scanActivityBase(row, &activity); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if err := d.hydrateActivity(&activity, false); err != nil {
		return nil, err
	}
	return &activity, nil
}

// ExcludeActivityEvidence removes one event from an activity's computed
// summary while preserving the raw event and an attributable correction row.
func (d *DB) ExcludeActivityEvidence(activityID, eventID, reason, actor string) (*Activity, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM activities WHERE id = ?`, activityID).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, ErrActivityNotFound
	}
	if err := tx.QueryRow(`
		SELECT COUNT(*) FROM activity_event_corrections
		WHERE activity_id = ? AND event_id = ? AND restored_at IS NULL`, activityID, eventID).Scan(&exists); err != nil {
		return nil, err
	}
	if exists > 0 {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return d.GetActivityByID(activityID)
	}
	if err := tx.QueryRow(`
		SELECT COUNT(*) FROM activity_events
		WHERE activity_id = ? AND event_id = ?`, activityID, eventID).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, ErrActivityEvidenceMissing
	}
	var evidenceCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM activity_events WHERE activity_id = ?`, activityID).Scan(&evidenceCount); err != nil {
		return nil, err
	}
	if evidenceCount <= 1 {
		return nil, ErrActivityNeedsEvidence
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "Does not belong to this activity"
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		actor = "local"
	}
	if _, err := tx.Exec(`
		INSERT INTO activity_event_corrections
			(activity_id, event_id, action, reason, actor, created_at)
		VALUES (?, ?, 'excluded', ?, ?, ?)`, activityID, eventID, reason, actor, utc(time.Now())); err != nil {
		return nil, fmt.Errorf("record activity evidence correction: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM activity_events WHERE activity_id = ? AND event_id = ?`, activityID, eventID); err != nil {
		return nil, fmt.Errorf("exclude activity evidence: %w", err)
	}
	survivingID, err := reconcileActivityTx(tx, activityID)
	if err != nil {
		return nil, err
	}
	if survivingID == "" {
		return nil, ErrActivityNeedsEvidence
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return d.GetActivityByID(survivingID)
}

// RestoreActivityEvidence reverses the active exclusion without erasing its
// audit history. Reconciliation safely merges any incident boundary the event
// bridges, but does not reopen or re-notify a finalized incident.
func (d *DB) RestoreActivityEvidence(activityID, eventID, actor string) (*Activity, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var activityExists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM activities WHERE id = ?`, activityID).Scan(&activityExists); err != nil {
		return nil, err
	}
	if activityExists == 0 {
		return nil, ErrActivityNotFound
	}
	var correctionID int64
	if err := tx.QueryRow(`
		SELECT id FROM activity_event_corrections
		WHERE activity_id = ? AND event_id = ? AND restored_at IS NULL`, activityID, eventID).Scan(&correctionID); err == sql.ErrNoRows {
		return nil, ErrActivityEvidenceMissing
	} else if err != nil {
		return nil, err
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		actor = "local"
	}
	now := utc(time.Now())
	if _, err := tx.Exec(`
		UPDATE activity_event_corrections
		SET restored_at = ?, restored_by = ? WHERE id = ?`, now, actor, correctionID); err != nil {
		return nil, fmt.Errorf("restore activity correction history: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO activity_events (activity_id, event_id) VALUES (?, ?)`, activityID, eventID); err != nil {
		return nil, fmt.Errorf("restore activity evidence: %w", err)
	}
	survivingID, err := reconcileActivityTx(tx, activityID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return d.GetActivityByID(survivingID)
}

// FinalizeDueActivities closes open incidents whose quiet period has elapsed.
func (d *DB) FinalizeDueActivities(now time.Time, limit int) ([]Activity, error) {
	if limit <= 0 {
		limit = 100
	}
	tx, err := d.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.Query(`
		SELECT id FROM activities
		WHERE state = ? AND end_time <= ?
		ORDER BY end_time ASC, id ASC LIMIT ?`, ActivityStateOpen, utc(now.Add(-activityMergeGap)), limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	finalizedAt := utc(now)
	for _, id := range ids {
		if _, err := tx.Exec(`
			UPDATE activities SET state = ?, finalized_at = ?, updated_at = ?
			WHERE id = ? AND state = ?`, ActivityStateFinalized, finalizedAt, finalizedAt, id, ActivityStateOpen); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	activities := make([]Activity, 0, len(ids))
	for _, id := range ids {
		activity, err := d.GetActivityByID(id)
		if err != nil {
			return nil, err
		}
		if activity != nil {
			activities = append(activities, *activity)
		}
	}
	return activities, nil
}

// PendingActivityNotifications returns finalized alert incidents that have not
// yet been accepted by the notification queue.
func (d *DB) PendingActivityNotifications(limit int) ([]Activity, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.db.Query(`
		SELECT `+activitySelectCols+`
		FROM activities a
		WHERE a.state = ? AND a.category != ? AND a.notification_queued_at IS NULL
		ORDER BY a.finalized_at ASC, a.id ASC LIMIT ?`, ActivityStateFinalized, camera.CategoryDetection, limit)
	if err != nil {
		return nil, err
	}
	var activities []Activity
	for rows.Next() {
		var activity Activity
		if err := scanActivityBase(rows, &activity); err != nil {
			_ = rows.Close()
			return nil, err
		}
		activities = append(activities, activity)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range activities {
		if err := d.hydrateActivity(&activities[i], false); err != nil {
			return nil, err
		}
	}
	return activities, nil
}

func (d *DB) MarkActivityNotificationQueued(id string, at time.Time) (bool, error) {
	result, err := d.db.Exec(`
		UPDATE activities SET notification_queued_at = ?, updated_at = ?
		WHERE id = ? AND state = ? AND notification_queued_at IS NULL`,
		utc(at), utc(at), id, ActivityStateFinalized)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
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
		correctionRows, err := d.db.Query(`
			SELECT event_id, action, reason, actor, created_at
			FROM activity_event_corrections
			WHERE activity_id = ? AND restored_at IS NULL
			ORDER BY created_at ASC, id ASC`, activity.ID)
		if err != nil {
			return err
		}
		type correctionRow struct {
			eventID, action, reason, actor string
			createdAt                      time.Time
		}
		var corrections []correctionRow
		for correctionRows.Next() {
			var correction correctionRow
			if err := correctionRows.Scan(&correction.eventID, &correction.action, &correction.reason, &correction.actor, &correction.createdAt); err != nil {
				_ = correctionRows.Close()
				return err
			}
			corrections = append(corrections, correction)
		}
		if err := correctionRows.Err(); err != nil {
			_ = correctionRows.Close()
			return err
		}
		if err := correctionRows.Close(); err != nil {
			return err
		}
		for _, correction := range corrections {
			event, err := d.GetEventByID(correction.eventID)
			if err != nil {
				return err
			}
			if event == nil {
				continue
			}
			activity.ExcludedEvidence = append(activity.ExcludedEvidence, ActivityEvidenceCorrection{
				Event: *event, Action: correction.action, Reason: correction.reason,
				Actor: correction.actor, CreatedAt: correction.createdAt,
			})
		}
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
