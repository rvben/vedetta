package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/storage"
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
			Camera     string    `json:"camera"`
			EventCount int       `json:"event_count"`
			State      string    `json:"state"`
			ClosesAt   time.Time `json:"closes_at"`
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
	if body.Items[0].State != "open" || body.Items[0].ClosesAt.IsZero() {
		t.Fatalf("missing lifecycle fields: %+v", body.Items[0])
	}
	if body.HasMore {
		t.Error("has_more = true, want false")
	}

	req = httptest.NewRequest(http.MethodGet, "/api/activities?state=finalized", nil)
	rec = httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("state filter status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Total != 0 || len(body.Items) != 0 {
		t.Fatalf("finalized filter returned open activities: %+v", body)
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
		HasDoorbell    bool                     `json:"has_doorbell"`
		MissedDoorbell bool                     `json:"missed_doorbell"`
		Grouping       storage.ActivityGrouping `json:"grouping"`
		Events         []camera.Event           `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.HasDoorbell || !body.MissedDoorbell || len(body.Events) != 1 {
		t.Fatalf("unexpected activity detail: %+v", body)
	}
	if body.Grouping.Strategy != "camera_local_quiet_period" || body.Grouping.QuietPeriodSeconds != 90 {
		t.Fatalf("unexpected grouping explanation: %+v", body.Grouping)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/activities/missing", nil)
	rec = httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d", rec.Code)
	}
}

func TestActivityEvidenceCorrectionAPIExcludesAndRestores(t *testing.T) {
	srv, db := newTestServer(t)
	start := time.Now().Add(-time.Hour).UTC()
	seedEvent(t, db, "person", "front_door", "person", 0.92, start)
	seedEvent(t, db, "car", "front_door", "car", 0.84, start.Add(30*time.Second))

	req := httptest.NewRequest(http.MethodPost,
		"/api/activities/act_person/evidence/car/exclude",
		strings.NewReader(`{"reason":"Wrong incident"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("exclude status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var corrected storage.Activity
	if err := json.Unmarshal(rec.Body.Bytes(), &corrected); err != nil {
		t.Fatal(err)
	}
	if corrected.EventCount != 1 || len(corrected.ExcludedEvidence) != 1 {
		t.Fatalf("corrected activity = %+v", corrected)
	}
	if correction := corrected.ExcludedEvidence[0]; correction.Event.ID != "car" || correction.Reason != "Wrong incident" || correction.Actor != "local" {
		t.Fatalf("correction = %+v", correction)
	}

	req = httptest.NewRequest(http.MethodPost,
		"/api/activities/act_person/evidence/car/restore", nil)
	rec = httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var restored storage.Activity
	if err := json.Unmarshal(rec.Body.Bytes(), &restored); err != nil {
		t.Fatal(err)
	}
	if restored.EventCount != 2 || len(restored.ExcludedEvidence) != 0 {
		t.Fatalf("restored activity = %+v", restored)
	}
}

func TestActivityEvidenceCorrectionAPIProtectsSoleEvidence(t *testing.T) {
	srv, db := newTestServer(t)
	seedEvent(t, db, "only", "front_door", "person", 0.92, time.Now().Add(-time.Hour).UTC())

	req := httptest.NewRequest(http.MethodPost,
		"/api/activities/act_only/evidence/only/exclude", nil)
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
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
		Total     int `json:"total"`
		Today     int `json:"today"`
		Open      int `json:"open"`
		Finalized int `json:"finalized"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Total != 2 || body.Today != 1 || body.Open != 2 || body.Finalized != 0 {
		t.Fatalf("counts = %+v, want total=2 today=1 open=2 finalized=0", body)
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
	for _, want := range []string{"activity-card", "Alex", "2 events", "/activity.html?id=act_person", "data-event-time=", "Collecting evidence"} {
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
		"Why these belong together",
		"no more than 90 seconds apart",
		"Exclude Person evidence from this activity",
		"/event.html?id=person&amp;activity=act_person&amp;camera=front_door&amp;q=Alex",
		"/event.html?id=car&amp;activity=act_person&amp;camera=front_door&amp;q=Alex",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("activity detail missing %q: %s", want, rec.Body.String())
		}
	}

	if _, err := db.ExcludeActivityEvidence("act_person", "car", "Wrong incident", "alex"); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/partials/activity/act_person", nil)
	rec = httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	for _, want := range []string{"Excluded evidence", "Wrong incident · by alex", "Restore Car evidence to this activity"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("corrected activity detail missing %q: %s", want, rec.Body.String())
		}
	}
}

func TestBroadcastActivitySSE(t *testing.T) {
	srv, _ := newTestServer(t)
	client := make(chan []byte, 1)
	srv.sseMu.Lock()
	srv.sseClients[client] = struct{}{}
	srv.sseMu.Unlock()
	srv.BroadcastActivitySSE("activity_finalized", storage.Activity{
		ID: "act_1", State: storage.ActivityStateFinalized,
	})
	select {
	case message := <-client:
		body := string(message)
		if !strings.Contains(body, "event: activity_finalized") || !strings.Contains(body, `"id":"act_1"`) {
			t.Fatalf("SSE message = %q", body)
		}
	case <-time.After(time.Second):
		t.Fatal("activity SSE was not broadcast")
	}
}
