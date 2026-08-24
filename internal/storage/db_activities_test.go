package storage

import (
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/camera"
)

func activityEvent(id, cameraName, label string, at time.Time) camera.Event {
	return camera.Event{
		ID:         id,
		CameraName: cameraName,
		Label:      label,
		Score:      0.8,
		Box:        [4]int{10, 10, 100, 100},
		Timestamp:  at,
		Category:   camera.CategoryAlert,
		Kind:       camera.EventKindObject,
	}
}

func TestActivityAggregationGroupsNearbyEvidence(t *testing.T) {
	db := newTestDB(t)
	start := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	person := activityEvent("person-1", "front_door", "person", start)
	person.ZoneName = "approach"
	person.SubLabel = "Alex"
	car := activityEvent("car-1", "front_door", "car", start.Add(40*time.Second))
	car.Category = camera.CategoryDetection
	car.SnapshotAvailable = true

	mustSaveEvent(t, db, person)
	mustSaveEvent(t, db, car)

	activities, err := db.QueryActivitiesFiltered(ActivityFilters{}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(activities) != 1 {
		t.Fatalf("activities = %d, want 1", len(activities))
	}
	activity := activities[0]
	if activity.ID != "act_person-1" {
		t.Errorf("ID = %q, want act_person-1", activity.ID)
	}
	if activity.State != ActivityStateOpen {
		t.Errorf("State = %q, want open", activity.State)
	}
	if !activity.ClosesAt.Equal(activity.EndTime.Add(activityMergeGap)) {
		t.Errorf("ClosesAt = %s, want %s", activity.ClosesAt, activity.EndTime.Add(activityMergeGap))
	}
	if activity.EventCount != 2 {
		t.Errorf("EventCount = %d, want 2", activity.EventCount)
	}
	if activity.Category != camera.CategoryAlert {
		t.Errorf("Category = %q, want alert", activity.Category)
	}
	if activity.PrimaryEvent.ID != "car-1" {
		t.Errorf("PrimaryEvent = %q, want snapshot-bearing car-1", activity.PrimaryEvent.ID)
	}
	if len(activity.Labels) != 2 || activity.Labels[0] != "car" || activity.Labels[1] != "person" {
		t.Errorf("Labels = %v, want [car person]", activity.Labels)
	}
	if len(activity.RecognizedNames) != 1 || activity.RecognizedNames[0] != "Alex" {
		t.Errorf("RecognizedNames = %v, want [Alex]", activity.RecognizedNames)
	}
}

func TestActivityAggregationSeparatesQuietPeriodsAndCameras(t *testing.T) {
	db := newTestDB(t)
	start := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	mustSaveEvent(t, db, activityEvent("front-1", "front_door", "person", start))
	mustSaveEvent(t, db, activityEvent("front-2", "front_door", "person", start.Add(activityMergeGap+time.Second)))
	mustSaveEvent(t, db, activityEvent("garden-1", "garden", "person", start.Add(10*time.Second)))

	activities, err := db.QueryActivitiesFiltered(ActivityFilters{}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(activities) != 3 {
		t.Fatalf("activities = %d, want 3", len(activities))
	}
}

func TestActivityAggregationLateBridgeMergesEpisodes(t *testing.T) {
	db := newTestDB(t)
	start := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	mustSaveEvent(t, db, activityEvent("first", "front_door", "person", start))
	mustSaveEvent(t, db, activityEvent("third", "front_door", "car", start.Add(3*time.Minute)))
	mustSaveEvent(t, db, activityEvent("bridge", "front_door", "dog", start.Add(90*time.Second)))

	activities, err := db.QueryActivitiesFiltered(ActivityFilters{}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(activities) != 1 {
		t.Fatalf("activities = %d, want 1", len(activities))
	}
	if activities[0].ID != "act_first" || activities[0].EventCount != 3 {
		t.Fatalf("merged activity = %+v, want canonical act_first with 3 events", activities[0])
	}
}

func TestActivityEndUpdateCanMergeNextEpisode(t *testing.T) {
	db := newTestDB(t)
	start := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	mustSaveEvent(t, db, activityEvent("long", "driveway", "person", start))
	mustSaveEvent(t, db, activityEvent("next", "driveway", "car", start.Add(3*time.Minute)))

	if err := db.UpdateEventEndTime("long", start.Add(90*time.Second)); err != nil {
		t.Fatal(err)
	}
	activities, err := db.QueryActivitiesFiltered(ActivityFilters{}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(activities) != 1 || activities[0].EventCount != 2 {
		t.Fatalf("activities = %+v, want one merged activity", activities)
	}
}

func TestActivityFiltersMatchEvidenceAndKeepFullContext(t *testing.T) {
	db := newTestDB(t)
	start := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	person := activityEvent("person", "front_door", "person", start)
	person.ZoneName = "porch"
	person.SubLabel = "Sam"
	car := activityEvent("car", "front_door", "car", start.Add(20*time.Second))
	mustSaveEvent(t, db, person)
	mustSaveEvent(t, db, car)

	activities, err := db.QueryActivitiesFiltered(ActivityFilters{Label: "person", Object: "Sam", Search: "porch"}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(activities) != 1 || activities[0].EventCount != 2 {
		t.Fatalf("filtered activities = %+v, want full two-event context", activities)
	}
	count, err := db.CountActivitiesFiltered(ActivityFilters{Label: "dog"})
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("dog activity count = %d, want 0", count)
	}
}

func TestGetActivityIncludesOrderedEvidence(t *testing.T) {
	db := newTestDB(t)
	start := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	mustSaveEvent(t, db, activityEvent("later", "front_door", "car", start.Add(20*time.Second)))
	mustSaveEvent(t, db, activityEvent("earlier", "front_door", "person", start))

	activity, err := db.GetActivityByID("act_later")
	if err != nil {
		t.Fatal(err)
	}
	// The first inserted event owns the stable ID even when older evidence
	// arrives later; evidence ordering itself is chronological.
	if activity == nil || len(activity.Events) != 2 {
		t.Fatalf("activity = %+v, want two evidence events", activity)
	}
	if activity.Events[0].ID != "earlier" || activity.Events[1].ID != "later" {
		t.Errorf("evidence order = [%s %s], want [earlier later]", activity.Events[0].ID, activity.Events[1].ID)
	}
}

func TestActivityEvidenceCorrectionExcludesAndRestoresRawEvent(t *testing.T) {
	db := newTestDB(t)
	start := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	for _, event := range []camera.Event{
		activityEvent("first", "front_door", "person", start),
		activityEvent("second", "front_door", "car", start.Add(30*time.Second)),
		activityEvent("unrelated", "front_door", "cat", start.Add(60*time.Second)),
	} {
		mustSaveEvent(t, db, event)
	}

	activity, err := db.ExcludeActivityEvidence("act_first", "unrelated", "Wrong detection", "alex")
	if err != nil {
		t.Fatal(err)
	}
	if activity.EventCount != 2 || len(activity.Events) != 2 {
		t.Fatalf("corrected activity = %+v, want two included events", activity)
	}
	if !activity.EndTime.Equal(start.Add(30 * time.Second)) {
		t.Errorf("corrected end = %s, want %s", activity.EndTime, start.Add(30*time.Second))
	}
	if len(activity.ExcludedEvidence) != 1 {
		t.Fatalf("excluded evidence = %+v, want one correction", activity.ExcludedEvidence)
	}
	correction := activity.ExcludedEvidence[0]
	if correction.Event.ID != "unrelated" || correction.Actor != "alex" || correction.Reason != "Wrong detection" {
		t.Errorf("correction = %+v", correction)
	}
	if raw, err := db.GetEventByID("unrelated"); err != nil || raw == nil {
		t.Fatalf("raw event was not preserved: event=%+v err=%v", raw, err)
	}

	restored, err := db.RestoreActivityEvidence("act_first", "unrelated", "sam")
	if err != nil {
		t.Fatal(err)
	}
	if restored.EventCount != 3 || len(restored.ExcludedEvidence) != 0 {
		t.Fatalf("restored activity = %+v, want all evidence active", restored)
	}
	var restoredBy string
	var restoredAt time.Time
	if err := db.db.QueryRow(`
		SELECT restored_by, restored_at FROM activity_event_corrections
		WHERE event_id = 'unrelated'`).Scan(&restoredBy, &restoredAt); err != nil {
		t.Fatal(err)
	}
	if restoredBy != "sam" || restoredAt.IsZero() {
		t.Errorf("restore audit = %q at %s", restoredBy, restoredAt)
	}
}

func TestActivityEvidenceCorrectionRequiresOneIncludedEvent(t *testing.T) {
	db := newTestDB(t)
	mustSaveEvent(t, db, activityEvent("only", "front_door", "person", time.Now()))

	if _, err := db.ExcludeActivityEvidence("act_only", "only", "", "operator"); err != ErrActivityNeedsEvidence {
		t.Fatalf("exclude sole evidence error = %v, want %v", err, ErrActivityNeedsEvidence)
	}
	activity, err := db.GetActivityByID("act_only")
	if err != nil {
		t.Fatal(err)
	}
	if activity == nil || activity.EventCount != 1 || len(activity.ExcludedEvidence) != 0 {
		t.Fatalf("sole-evidence activity changed: %+v", activity)
	}
}

func TestActivityMergeMovesCorrectionHistoryToCanonicalIncident(t *testing.T) {
	db := newTestDB(t)
	start := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	mustSaveEvent(t, db, activityEvent("early", "driveway", "person", start))
	mustSaveEvent(t, db, activityEvent("late-first", "driveway", "car", start.Add(3*time.Minute)))
	mustSaveEvent(t, db, activityEvent("late-extra", "driveway", "cat", start.Add(200*time.Second)))
	if _, err := db.ExcludeActivityEvidence("act_late-first", "late-extra", "Unrelated", "operator"); err != nil {
		t.Fatal(err)
	}

	// This late-arriving event sits exactly one quiet period from both
	// incidents, so the normal aggregator merges them.
	mustSaveEvent(t, db, activityEvent("bridge", "driveway", "dog", start.Add(90*time.Second)))
	activity, err := db.GetActivityByID("act_early")
	if err != nil {
		t.Fatal(err)
	}
	if activity == nil || activity.EventCount != 3 || len(activity.ExcludedEvidence) != 1 {
		t.Fatalf("merged activity = %+v", activity)
	}
	if activity.ExcludedEvidence[0].Event.ID != "late-extra" {
		t.Errorf("moved correction = %+v", activity.ExcludedEvidence[0])
	}
}

func TestDeleteEventRemovesEmptyActivity(t *testing.T) {
	db := newTestDB(t)
	mustSaveEvent(t, db, activityEvent("only", "front_door", "person", time.Now()))
	if err := db.DeleteEvent("only"); err != nil {
		t.Fatal(err)
	}
	activity, err := db.GetActivityByID("act_only")
	if err != nil {
		t.Fatal(err)
	}
	if activity != nil {
		t.Fatalf("activity survived its final evidence deletion: %+v", activity)
	}
}

func TestMigrateV7BackfillsActivities(t *testing.T) {
	raw, _ := openRaw(t)
	if _, err := raw.Exec(baselineSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 6`); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		id    string
		at    time.Time
		label string
	}{{"old-1", start, "person"}, {"old-2", start.Add(20 * time.Second), "car"}} {
		if _, err := raw.Exec(`
			INSERT INTO events (id, camera, label, score, timestamp, category, kind)
			VALUES (?, 'front_door', ?, 0.8, ?, 'alert', 'object')`, row.id, row.label, row.at); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(raw); err != nil {
		t.Fatal(err)
	}
	wrapper := &DB{db: raw}
	activities, err := wrapper.QueryActivitiesFiltered(ActivityFilters{}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(activities) != 1 || activities[0].EventCount != 2 {
		t.Fatalf("backfilled activities = %+v, want one with two events", activities)
	}
	if activities[0].State != ActivityStateFinalized || activities[0].FinalizedAt == nil {
		t.Fatalf("backfilled lifecycle = %+v, want finalized historical activity", activities[0])
	}
}

func TestMigrateV8AddsLifecycleColumnsToExistingActivities(t *testing.T) {
	raw, _ := openRaw(t)
	if _, err := raw.Exec(baselineSchema); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"notification_queued_at", "finalized_at", "state"} {
		if _, err := raw.Exec(`ALTER TABLE activities DROP COLUMN ` + column); err != nil {
			t.Fatalf("drop %s: %v", column, err)
		}
	}
	if _, err := raw.Exec(`
		INSERT INTO activities (id, camera, start_time, end_time, category)
		VALUES ('act_old', 'front_door', '2026-08-23 10:00:00', '2026-08-23 10:00:10', 'alert');
		PRAGMA user_version = 7;`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(raw); err != nil {
		t.Fatal(err)
	}
	var state string
	var finalizedAt, queuedAt time.Time
	if err := raw.QueryRow(`
		SELECT state, finalized_at, notification_queued_at
		FROM activities WHERE id = 'act_old'`).Scan(&state, &finalizedAt, &queuedAt); err != nil {
		t.Fatal(err)
	}
	if state != string(ActivityStateFinalized) || finalizedAt.IsZero() || queuedAt.IsZero() {
		t.Fatalf("migrated lifecycle = %q, %s, %s", state, finalizedAt, queuedAt)
	}
}

func TestMigrateV9AddsActivityEvidenceCorrectionHistory(t *testing.T) {
	raw, _ := openRaw(t)
	if _, err := raw.Exec(baselineSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TABLE activity_event_corrections; PRAGMA user_version = 8;`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(raw); err != nil {
		t.Fatal(err)
	}
	var table string
	if err := raw.QueryRow(`
		SELECT name FROM sqlite_master
		WHERE type = 'table' AND name = 'activity_event_corrections'`).Scan(&table); err != nil {
		t.Fatal(err)
	}
	if table != "activity_event_corrections" {
		t.Fatalf("migrated table = %q", table)
	}
}

func TestActivityLifecycleFinalizesAfterQuietPeriod(t *testing.T) {
	db := newTestDB(t)
	start := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	mustSaveEvent(t, db, activityEvent("person", "front_door", "person", start))

	finalized, err := db.FinalizeDueActivities(start.Add(activityMergeGap-time.Second), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(finalized) != 0 {
		t.Fatalf("finalized early = %+v", finalized)
	}

	finalized, err = db.FinalizeDueActivities(start.Add(activityMergeGap+time.Second), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(finalized) != 1 || finalized[0].State != ActivityStateFinalized || finalized[0].FinalizedAt == nil {
		t.Fatalf("finalized = %+v, want one finalized activity", finalized)
	}
	openCount, err := db.CountActivitiesFiltered(ActivityFilters{State: ActivityStateOpen})
	if err != nil {
		t.Fatal(err)
	}
	finalCount, err := db.CountActivitiesFiltered(ActivityFilters{State: ActivityStateFinalized})
	if err != nil {
		t.Fatal(err)
	}
	if openCount != 0 || finalCount != 1 {
		t.Fatalf("state counts = open %d, finalized %d", openCount, finalCount)
	}
}

func TestLateEvidenceReopensWithoutDuplicateNotification(t *testing.T) {
	db := newTestDB(t)
	start := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	mustSaveEvent(t, db, activityEvent("first", "front_door", "person", start))
	if _, err := db.FinalizeDueActivities(start.Add(activityMergeGap+time.Second), 10); err != nil {
		t.Fatal(err)
	}
	marked, err := db.MarkActivityNotificationQueued("act_first", start.Add(2*time.Minute))
	if err != nil || !marked {
		t.Fatalf("mark queued = %v, %v", marked, err)
	}

	mustSaveEvent(t, db, activityEvent("late", "front_door", "car", start.Add(30*time.Second)))
	activity, err := db.GetActivityByID("act_first")
	if err != nil {
		t.Fatal(err)
	}
	if activity == nil || activity.State != ActivityStateOpen || activity.FinalizedAt != nil || activity.EventCount != 2 {
		t.Fatalf("reopened activity = %+v", activity)
	}
	if _, err := db.FinalizeDueActivities(start.Add(3*time.Minute), 10); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingActivityNotifications(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("late evidence queued a duplicate notification: %+v", pending)
	}
}

func TestPendingActivityNotificationsAreClaimedOnce(t *testing.T) {
	db := newTestDB(t)
	start := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	mustSaveEvent(t, db, activityEvent("alert", "front_door", "person", start))
	detection := activityEvent("detection", "garden", "cat", start)
	detection.Category = camera.CategoryDetection
	mustSaveEvent(t, db, detection)
	if _, err := db.FinalizeDueActivities(start.Add(2*time.Minute), 10); err != nil {
		t.Fatal(err)
	}

	pending, err := db.PendingActivityNotifications(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != "act_alert" {
		t.Fatalf("pending = %+v, want only alert activity", pending)
	}
	marked, err := db.MarkActivityNotificationQueued("act_alert", start.Add(2*time.Minute))
	if err != nil || !marked {
		t.Fatalf("first mark = %v, %v", marked, err)
	}
	marked, err = db.MarkActivityNotificationQueued("act_alert", start.Add(3*time.Minute))
	if err != nil || marked {
		t.Fatalf("second mark = %v, %v", marked, err)
	}
	pending, err = db.PendingActivityNotifications(10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after claim = %+v, %v", pending, err)
	}
}
