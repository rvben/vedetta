package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/camera"
)

// The camera grid renders a "last seen" caption on offline tiles. ListCameras
// must therefore expose the last-known snapshot time so the dashboard can
// format it client-side.
func TestListCamerasIncludesLastSeen(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := time.Date(2026, 5, 31, 9, 15, 0, 0, time.UTC)
	cam := camera.NewTestCamera("front")
	cam.SetTestOnline(false)
	cam.SetTestLastFrameTime(ts)
	srv.cameras.RegisterForTest(cam)

	req := httptest.NewRequest(http.MethodGet, "/api/cameras", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	var env struct {
		Items []struct {
			Name     string `json:"name"`
			LastSeen string `json:"last_seen"`
		} `json:"items"`
	}
	if err := json.NewDecoder(w.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var found bool
	for _, it := range env.Items {
		if it.Name == "front" {
			found = true
			want := ts.UTC().Format(time.RFC3339)
			if it.LastSeen != want {
				t.Errorf("last_seen = %q, want %q", it.LastSeen, want)
			}
		}
	}
	if !found {
		t.Fatal("camera 'front' missing from /api/cameras response")
	}
}

// A camera that has never produced a frame has no last-known snapshot, so the
// field is omitted rather than emitting a zero timestamp the UI would
// misformat as "last seen 56 years ago".
func TestListCamerasOmitsLastSeenWhenNeverSeen(t *testing.T) {
	srv, _ := newTestServer(t)
	cam := camera.NewTestCamera("front")
	cam.SetTestOnline(false)
	srv.cameras.RegisterForTest(cam)

	req := httptest.NewRequest(http.MethodGet, "/api/cameras", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	var env struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.NewDecoder(w.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, it := range env.Items {
		if it["name"] == "front" {
			if v, present := it["last_seen"]; present {
				t.Errorf("last_seen present (%v) for a camera that never produced a frame; want omitted", v)
			}
		}
	}
}

// The detail endpoint drives the immediate sleeping/offline camera-page state,
// so it needs the same last-known timestamp as the grid. last_frame alone is
// insufficient after restart because a cached snapshot can outlive live decode.
func TestGetCameraIncludesLastSeen(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := time.Date(2026, 8, 12, 18, 52, 18, 0, time.UTC)
	cam := camera.NewTestCamera("driveway")
	cam.SetTestOnline(false)
	cam.SetTestLastFrameTime(ts)
	srv.cameras.RegisterForTest(cam)

	req := httptest.NewRequest(http.MethodGet, "/api/cameras/driveway", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	var detail map[string]any
	if err := json.NewDecoder(w.Body).Decode(&detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := ts.Format(time.RFC3339)
	if got := detail["last_seen"]; got != want {
		t.Errorf("last_seen = %v, want %q", got, want)
	}
}

func TestGetCameraOmitsLastSeenWhenNeverSeen(t *testing.T) {
	srv, _ := newTestServer(t)
	cam := camera.NewTestCamera("new-camera")
	cam.SetTestOnline(false)
	srv.cameras.RegisterForTest(cam)

	req := httptest.NewRequest(http.MethodGet, "/api/cameras/new-camera", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	var detail map[string]any
	if err := json.NewDecoder(w.Body).Decode(&detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if value, present := detail["last_seen"]; present {
		t.Errorf("last_seen present (%v) for a camera that never produced a frame; want omitted", value)
	}
}
