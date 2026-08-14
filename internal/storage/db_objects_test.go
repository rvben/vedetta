package storage

import (
	"testing"
	"time"
)

func TestSaveObjectSightingRequiresRetainedEvent(t *testing.T) {
	db := newTestDB(t)
	objectID, err := db.SaveKnownObject(KnownObject{Name: "Family bike", Label: "bicycle"})
	if err != nil {
		t.Fatalf("SaveKnownObject: %v", err)
	}
	now := time.Now()

	for _, eventID := range []string{"", "missing-event"} {
		if _, err := db.SaveObjectSighting(ObjectSighting{
			EventID: eventID, ObjectID: objectID, Camera: "garage", Similarity: 0.82, Timestamp: now,
		}); err == nil {
			t.Errorf("SaveObjectSighting(%q) succeeded without a retained event", eventID)
		}
	}

	sightings, err := db.ListObjectSightings(objectID, 0)
	if err != nil {
		t.Fatalf("ListObjectSightings: %v", err)
	}
	if len(sightings) != 0 {
		t.Fatalf("stored %d orphan sightings, want 0", len(sightings))
	}
}

func TestSaveObjectRecognitionLinksExistingEventAtomically(t *testing.T) {
	db := newTestDB(t)
	objectID, err := db.SaveKnownObject(KnownObject{Name: "Family bike", Label: "bicycle"})
	if err != nil {
		t.Fatalf("SaveKnownObject: %v", err)
	}
	now := time.Now()
	event := makeEvent("event-1", "garage", "bicycle", 0.9, now)
	mustSaveEvent(t, db, event)

	id, err := db.SaveObjectRecognition(ObjectSighting{
		EventID: "event-1", Camera: "garage", ObjectID: objectID,
		ObjectName: "Family bike", Similarity: 0.82, Timestamp: now,
	})
	if err != nil {
		t.Fatalf("SaveObjectRecognition: %v", err)
	}
	if id == 0 {
		t.Fatal("SaveObjectRecognition returned a zero sighting ID")
	}

	storedEvent, err := db.GetEventByID("event-1")
	if err != nil {
		t.Fatalf("GetEventByID: %v", err)
	}
	if storedEvent == nil || storedEvent.ObjectName != "Family bike" || storedEvent.SubLabel != "Family bike" {
		t.Fatalf("event identity not updated atomically: %+v", storedEvent)
	}
	sightings, err := db.ListObjectSightings(objectID, 0)
	if err != nil {
		t.Fatalf("ListObjectSightings: %v", err)
	}
	if len(sightings) != 1 || sightings[0].EventID != "event-1" {
		t.Fatalf("sightings = %+v, want one linked to event-1", sightings)
	}
}

func TestSaveObjectRecognitionRejectsMissingEventWithoutPartialWrite(t *testing.T) {
	db := newTestDB(t)
	objectID, err := db.SaveKnownObject(KnownObject{Name: "Family bike", Label: "bicycle"})
	if err != nil {
		t.Fatalf("SaveKnownObject: %v", err)
	}

	if _, err := db.SaveObjectRecognition(ObjectSighting{
		EventID: "missing-event", Camera: "garage", ObjectID: objectID,
		ObjectName: "Family bike", Similarity: 0.82, Timestamp: time.Now(),
	}); err == nil {
		t.Fatal("SaveObjectRecognition succeeded for a missing event")
	}

	sightings, err := db.ListObjectSightings(objectID, 0)
	if err != nil {
		t.Fatalf("ListObjectSightings: %v", err)
	}
	if len(sightings) != 0 {
		t.Fatalf("stored %d partial sightings, want 0", len(sightings))
	}
}
