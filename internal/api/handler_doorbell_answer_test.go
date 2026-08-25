package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/camera"
)

func TestAnswerDoorbell_FirstAnswererWins(t *testing.T) {
	s, db := newTestServer(t)
	if err := db.SaveEvent(camera.Event{
		ID:         "r1",
		CameraName: "front_door",
		Label:      "doorbell",
		Kind:       camera.EventKindDoorbell,
		Timestamp:  time.Now(),
	}); err != nil {
		t.Fatalf("save event: %v", err)
	}

	do := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/cameras/front_door/doorbell/r1/answer", nil)
		s.AnswerDoorbell(rec, req, "front_door", "r1")
		return rec
	}

	rec := do()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp["event_id"] != "r1" {
		t.Errorf("event_id = %v, want r1", resp["event_id"])
	}

	got, err := db.GetEventByID("r1")
	if err != nil {
		t.Fatalf("GetEventByID: %v", err)
	}
	if got.AnsweredAt.IsZero() {
		t.Error("ring not marked answered")
	}
	first := got.AnsweredAt

	// Second answer must be idempotent: answered_at must not change.
	_ = do()
	got2, _ := db.GetEventByID("r1")
	if !got2.AnsweredAt.Equal(first) {
		t.Error("second answer changed answered_at; should be idempotent")
	}
}

func TestAnswerDoorbell_NotFound(t *testing.T) {
	s, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/cameras/c/doorbell/missing/answer", nil)
	s.AnswerDoorbell(rec, req, "c", "missing")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body.String())
	}
}

func TestAnswerDoorbell_RejectsNonDoorbell(t *testing.T) {
	s, db := newTestServer(t)
	if err := db.SaveEvent(camera.Event{
		ID:         "obj1",
		CameraName: "c",
		Label:      "person",
		Kind:       camera.EventKindObject,
		Timestamp:  time.Now(),
	}); err != nil {
		t.Fatalf("save event: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/cameras/c/doorbell/obj1/answer", nil)
	s.AnswerDoorbell(rec, req, "c", "obj1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for non-doorbell event; body: %s", rec.Code, rec.Body.String())
	}
}

func TestAnswerDoorbell_RejectsMismatchedCamera(t *testing.T) {
	s, db := newTestServer(t)
	if err := db.SaveEvent(camera.Event{ID: "r2", CameraName: "front_door", Label: "doorbell", Kind: camera.EventKindDoorbell, Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/cameras/back_door/doorbell/r2/answer", nil)
	s.AnswerDoorbell(rec, req, "back_door", "r2")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
