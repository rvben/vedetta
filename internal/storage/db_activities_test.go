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
}
