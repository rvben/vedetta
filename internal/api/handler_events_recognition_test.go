package api

import (
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/detect"
	"github.com/rvben/vedetta/internal/storage"
)

func TestIdentifyEventAssignsOnlyTheBestKnownObject(t *testing.T) {
	fixture := newRecognitionEventFixture(t, "event-best-match")

	req := httptest.NewRequest(http.MethodPost, "/api/events/"+fixture.event.ID+"/identify", nil)
	w := httptest.NewRecorder()
	fixture.server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var sightings []storage.ObjectSighting
	if err := json.Unmarshal(w.Body.Bytes(), &sightings); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(sightings) != 1 || sightings[0].ObjectID != fixture.bestID {
		t.Fatalf("sightings = %+v, want one match for object %d", sightings, fixture.bestID)
	}
}

func TestIdentifyEventNamesTheEventWithTheBestKnownObject(t *testing.T) {
	fixture := newRecognitionEventFixture(t, "event-name")

	identifyReq := httptest.NewRequest(http.MethodPost, "/api/events/"+fixture.event.ID+"/identify", nil)
	identifyResponse := httptest.NewRecorder()
	fixture.server.mux.ServeHTTP(identifyResponse, identifyReq)
	if identifyResponse.Code != http.StatusOK {
		t.Fatalf("identify status = %d, want 200: %s", identifyResponse.Code, identifyResponse.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/events/"+fixture.event.ID, nil)
	getResponse := httptest.NewRecorder()
	fixture.server.mux.ServeHTTP(getResponse, getReq)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200: %s", getResponse.Code, getResponse.Body.String())
	}
	var event camera.Event
	if err := json.Unmarshal(getResponse.Body.Bytes(), &event); err != nil {
		t.Fatalf("decode event response: %v", err)
	}
	if event.ObjectName != "best" || event.SubLabel != "best" {
		t.Fatalf("event object_name=%q sub_label=%q, want both %q", event.ObjectName, event.SubLabel, "best")
	}
}

type recognitionEventFixture struct {
	server *Server
	event  camera.Event
	bestID int64
}

func newRecognitionEventFixture(t *testing.T, eventID string) recognitionEventFixture {
	t.Helper()
	srv, db := newTestServer(t)
	srv.objectEmbedder = &stubObjectEmbedder{embedRes: []float32{1, 0}}
	srv.ObjectMatchThreshold = 0.5

	snapshotPath := filepath.Join(t.TempDir(), "event.jpg")
	writeRecognitionTestSnapshot(t, snapshotPath)
	event := camera.Event{
		ID:                eventID,
		CameraName:        "driveway",
		Label:             "car",
		Box:               [4]int{0, 0, 2, 2},
		Timestamp:         time.Now(),
		SnapshotPath:      snapshotPath,
		SnapshotAvailable: true,
	}
	if err := db.SaveEvent(event); err != nil {
		t.Fatalf("SaveEvent: %v", err)
	}

	bestID, err := db.SaveKnownObject(storage.KnownObject{
		Name:     "best",
		Label:    "car",
		Centroid: detect.Float32ToBytes([]float32{0.9, 0.4358899}),
	})
	if err != nil {
		t.Fatalf("SaveKnownObject(best): %v", err)
	}
	_, err = db.SaveKnownObject(storage.KnownObject{
		Name:     "second",
		Label:    "car",
		Centroid: detect.Float32ToBytes([]float32{0.8, 0.6}),
	})
	if err != nil {
		t.Fatalf("SaveKnownObject(second): %v", err)
	}

	return recognitionEventFixture{server: srv, event: event, bestID: bestID}
}

func writeRecognitionTestSnapshot(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err := jpeg.Encode(f, img, nil); err != nil {
		_ = f.Close()
		t.Fatalf("encode snapshot: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close snapshot: %v", err)
	}
}
