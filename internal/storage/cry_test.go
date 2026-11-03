package storage

import (
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/camera"
)

func TestCryEventStaysOutsideVideoActivity(t *testing.T) {
	db, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ev := camera.Event{ID: "cry-1", CameraName: "nursery", Kind: camera.EventKindCry, Label: "baby_cry", Score: .8, Timestamp: time.Now(), Category: camera.CategoryAlert}
	if err := db.SaveEvent(ev); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateEventEndTime(ev.ID, ev.Timestamp.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	stored, err := db.GetEventByID(ev.ID)
	if err != nil || stored == nil || stored.Kind != camera.EventKindCry || stored.EndTime.IsZero() {
		t.Fatal("cry evidence not stored")
	}
	activity, err := db.GetActivityByEventID(ev.ID)
	if err != nil || activity != nil {
		t.Fatal("cry would produce delayed/duplicate activity notification")
	}
}
