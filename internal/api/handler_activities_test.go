package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/camera"
)

func TestListActivitiesReturnsGroupedReviewItems(t *testing.T) {
	srv, db := newTestServer(t)
	start := time.Now().Add(-time.Hour).UTC()
	seedEvent(t, db, "person", "front_door", "person", 0.92, start)
	seedEvent(t, db, "car", "front_door", "car", 0.84, start.Add(30*time.Second))
	seedEvent(t, db, "garden", "garden", "cat", 0.75, start.Add(10*time.Second))

	req := httptest.NewRequest(http.MethodGet, "/api/activities?camera=front_door&limit=1", nil)
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []struct {
			Camera     string `json:"camera"`
			EventCount int    `json:"event_count"`
		} `json:"items"`
		Total   int  `json:"total"`
		HasMore bool `json:"has_more"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Total != 1 || len(body.Items) != 1 || body.Items[0].Camera != "front_door" || body.Items[0].EventCount != 2 {
		t.Fatalf("unexpected activity response: %+v", body)
	}
	if body.HasMore {
		t.Error("has_more = true, want false")
	}
}

func TestGetActivityIncludesEvidenceAndNotFound(t *testing.T) {
	srv, db := newTestServer(t)
	start := time.Now().Add(-time.Hour).UTC()
	event := camera.Event{
		ID: "doorbell", CameraName: "front_door", Label: "person", Score: 0.9,
		Box: [4]int{10, 20, 100, 200}, Timestamp: start,
		Category: camera.CategoryAlert, Kind: camera.EventKindDoorbell,
	}
	if err := db.SaveEvent(event); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/activities/act_doorbell", nil)
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		HasDoorbell    bool           `json:"has_doorbell"`
		MissedDoorbell bool           `json:"missed_doorbell"`
		Events         []camera.Event `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.HasDoorbell || !body.MissedDoorbell || len(body.Events) != 1 {
		t.Fatalf("unexpected activity detail: %+v", body)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/activities/missing", nil)
	rec = httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d", rec.Code)
	}
}

func TestGetActivityCounts(t *testing.T) {
	srv, db := newTestServer(t)
	seedEvent(t, db, "today", "front", "person", 0.9, time.Now().Add(-time.Minute))
	seedEvent(t, db, "old", "front", "person", 0.9, time.Now().Add(-48*time.Hour))

	req := httptest.NewRequest(http.MethodGet, "/api/activities/counts", nil)
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Total int `json:"total"`
		Today int `json:"today"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Total != 2 || body.Today != 1 {
		t.Fatalf("counts = %+v, want total=2 today=1", body)
	}
}

func TestActivityPartialsExposeIncidentAndEvidence(t *testing.T) {
	srv, db := newTestServer(t)
	start := time.Now().Add(-time.Hour).UTC()
	for _, event := range []camera.Event{
		{ID: "person", CameraName: "front_door", Label: "person", SubLabel: "Alex", Score: 0.92, Box: [4]int{10, 20, 100, 200}, Timestamp: start},
		{ID: "car", CameraName: "front_door", Label: "car", Score: 0.84, Box: [4]int{10, 20, 100, 200}, Timestamp: start.Add(30 * time.Second)},
	} {
		if err := db.SaveEvent(event); err != nil {
			t.Fatal(err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/partials/activities-gallery", nil)
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("gallery status = %d, body = %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"activity-card", "Alex", "2 events", "/activity.html?id=act_person", "data-event-time="} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("activity gallery missing %q: %s", want, rec.Body.String())
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/partials/activity/act_person?camera=front_door&q=Alex", nil)
	rec = httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status = %d, body = %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{
		"activity-review-layout",
		"Activity summary",
		"Evidence",
		"/event.html?id=person&amp;activity=act_person&amp;camera=front_door&amp;q=Alex",
		"/event.html?id=car&amp;activity=act_person&amp;camera=front_door&amp;q=Alex",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("activity detail missing %q: %s", want, rec.Body.String())
		}
	}
}
