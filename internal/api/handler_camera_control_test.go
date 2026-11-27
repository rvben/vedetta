package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/config"
)

// A start that is refused because the camera's previous run has not finished
// must not record the camera as started: the stored flag decides whether the
// next process start brings the camera up.
func TestStartCameraKeepsTheStoppedFlagWhileThePreviousRunDrains(t *testing.T) {
	srv, db := newTestServer(t)
	srv.cameras.AddCamera(config.CameraConfig{Name: "test_cam", URL: "rtsp://localhost/stream"})
	if err := db.SetCameraStopped("test_cam", true); err != nil {
		t.Fatal(err)
	}
	finish := srv.cameras.HoldStoppingForTest("test_cam")

	start := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/cameras/test_cam/start", nil))
		return rec
	}
	storedAsStopped := func() bool {
		stopped, err := db.ListStoppedCameras()
		if err != nil {
			t.Fatal(err)
		}
		return slices.Contains(stopped, "test_cam")
	}

	rec := start()
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "still stopping") {
		t.Fatalf("start while draining = %d %s, want 409 saying the camera is still stopping", rec.Code, rec.Body.String())
	}
	if !storedAsStopped() {
		t.Fatal("a refused start cleared the stored stopped flag")
	}

	finish()
	if rec := start(); rec.Code != http.StatusOK {
		t.Fatalf("start after the previous run finished = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if storedAsStopped() {
		t.Fatal("a successful start left the camera stored as stopped")
	}
}

// A stop and a start each change the manager and then the stored flag. A stop
// that ran between the two steps of a start would have its flag cleared by
// that start, and the stopped camera would come back at the next process
// start. The second request therefore waits for the first.
func TestCameraStartAndStopDoNotInterleave(t *testing.T) {
	srv, db := newTestServer(t)
	srv.cameras.RegisterForTest(camera.NewTestCamera("test_cam"))
	storedAsStopped := func() bool {
		stopped, err := db.ListStoppedCameras()
		if err != nil {
			t.Fatal(err)
		}
		return slices.Contains(stopped, "test_cam")
	}

	// A start or stop that is under way holds the lock.
	srv.cameraControl.Lock()
	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/cameras/test_cam/stop", nil))
		done <- rec.Code
	}()
	select {
	case code := <-done:
		t.Fatalf("stop answered %d while another start or stop was under way", code)
	case <-time.After(200 * time.Millisecond):
	}
	if srv.cameras.IsStopped("test_cam") || storedAsStopped() {
		t.Fatal("stop changed the camera while another start or stop was under way")
	}

	srv.cameraControl.Unlock()
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("stop = %d, want 200", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stop did not finish once the other request had")
	}
	if !srv.cameras.IsStopped("test_cam") || !storedAsStopped() {
		t.Fatal("stop did not stop the camera and store it as stopped")
	}
}
