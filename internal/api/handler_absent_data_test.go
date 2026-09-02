package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/storage"
)

// dropTable removes a table so a single query fails while the rest of the
// database keeps working. It isolates one failing call the way closing the
// whole database cannot.
func dropTable(t *testing.T, db *storage.DB, name string) {
	t.Helper()
	if _, err := db.Raw().Exec("DROP TABLE " + name); err != nil {
		t.Fatalf("drop table %s: %v", name, err)
	}
}

// A database that cannot answer must not be rendered as a house where nothing
// happened. An unreadable events table is not "zero events".
func TestGetEventCounts_DatabaseErrorIsNotZero(t *testing.T) {
	s, db := newTestServer(t)
	seedEvent(t, db, "e1", "cam", "person", 0.9, time.Now().UTC())
	dropTable(t, db, "events")

	req := httptest.NewRequest(http.MethodGet, "/api/events/counts", nil)
	w := httptest.NewRecorder()
	s.GetEventCounts(w, req, GetEventCountsParams{})

	if w.Code == http.StatusOK {
		t.Fatalf("counts returned 200 for an unreadable database: %s", w.Body.String())
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500", w.Code, w.Body.String())
	}
}

// The face query backs face_count and the thumbnail choice. When it fails the
// answer is an error, not a person with zero faces.
func TestListPeople_FaceQueryErrorIsNotZero(t *testing.T) {
	s, db := newTestServer(t)
	if _, err := db.SavePerson("Alice", false, nil); err != nil {
		t.Fatalf("save person: %v", err)
	}
	dropTable(t, db, "faces")

	req := httptest.NewRequest(http.MethodGet, "/api/people", nil)
	w := httptest.NewRecorder()
	s.ListPeople(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500 when the face query fails", w.Code, w.Body.String())
	}
}

// Same for the appearance count: an unreadable events table must not render as
// "this person was never seen".
func TestListPeople_AppearanceCountErrorIsNotZero(t *testing.T) {
	s, db := newTestServer(t)
	if _, err := db.SavePerson("Alice", false, nil); err != nil {
		t.Fatalf("save person: %v", err)
	}
	dropTable(t, db, "events")

	req := httptest.NewRequest(http.MethodGet, "/api/people", nil)
	w := httptest.NewRecorder()
	s.ListPeople(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500 when the appearance count fails", w.Code, w.Body.String())
	}
}

func TestGetPerson_FaceQueryErrorIsNotZero(t *testing.T) {
	s, db := newTestServer(t)
	id, err := db.SavePerson("Alice", false, nil)
	if err != nil {
		t.Fatalf("save person: %v", err)
	}
	dropTable(t, db, "faces")

	req := httptest.NewRequest(http.MethodGet, "/api/people/1", nil)
	w := httptest.NewRecorder()
	s.GetPerson(w, req, id)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500 when the face query fails", w.Code, w.Body.String())
	}
}

// Positive control for the two tests above: with a healthy database the counts
// are the real numbers, so the 500s prove error handling and not a broken
// handler.
func TestListPeople_CountsAreAccurate(t *testing.T) {
	s, db := newTestServer(t)
	if _, err := db.SavePerson("Alice", false, nil); err != nil {
		t.Fatalf("save person: %v", err)
	}
	now := time.Now().UTC()
	for i, id := range []string{"a1", "a2", "a3"} {
		err := db.SaveEvent(camera.Event{
			ID:         id,
			CameraName: "cam",
			Label:      "person",
			ObjectName: "Alice",
			Timestamp:  now.Add(-time.Duration(i) * time.Minute),
		})
		if err != nil {
			t.Fatalf("save event %s: %v", id, err)
		}
	}
	// An event for someone else must not be counted.
	if err := db.SaveEvent(camera.Event{
		ID: "b1", CameraName: "cam", Label: "person", ObjectName: "Bob", Timestamp: now,
	}); err != nil {
		t.Fatalf("save event b1: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/people", nil)
	w := httptest.NewRecorder()
	s.ListPeople(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var out struct {
		Items []struct {
			Name            string `json:"name"`
			FaceCount       int    `json:"face_count"`
			AppearanceCount int    `json:"appearance_count"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("items=%d, want 1", len(out.Items))
	}
	if out.Items[0].AppearanceCount != 3 {
		t.Errorf("appearance_count=%d, want 3", out.Items[0].AppearanceCount)
	}
	if out.Items[0].FaceCount != 0 {
		t.Errorf("face_count=%d, want 0", out.Items[0].FaceCount)
	}
}
