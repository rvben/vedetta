package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/config"
)

// listCameraState fetches GET /api/cameras and returns the named camera's entry.
func listCameraState(t *testing.T, srv *Server, name string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/cameras", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var env struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.NewDecoder(w.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, it := range env.Items {
		if it["name"] == name {
			return it
		}
	}
	t.Fatalf("camera %q missing from /api/cameras response", name)
	return nil
}

func TestCameraGridOffersManualDoorbellOnlyForConfiguredDoorbells(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, name := range []string{"front-door", "side-door", "garage"} {
		cam := camera.NewTestCamera(name)
		cam.SetTestOnline(true)
		srv.cameras.RegisterForTest(cam)
	}
	if err := srv.cameras.StopCamera("side-door"); err != nil {
		t.Fatalf("StopCamera: %v", err)
	}
	srv.cameraConfigs = []config.CameraConfig{
		{Name: "front-door", Doorbell: config.DoorbellConfig{Enabled: true}},
		{Name: "side-door", Doorbell: config.DoorbellConfig{Enabled: true}},
		{Name: "garage"},
	}

	req := httptest.NewRequest(http.MethodGet, "/partials/camera-grid", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `href="/doorbell-answer.html?camera=front-door"`) {
		t.Fatalf("configured doorbell is missing manual entry action: %s", body)
	}
	if strings.Contains(body, `href="/doorbell-answer.html?camera=garage"`) {
		t.Fatal("ordinary camera must not expose the doorbell console")
	}
	if strings.Contains(body, `href="/doorbell-answer.html?camera=side-door"`) ||
		!strings.Contains(body, `title="Start camera to open doorbell"`) {
		t.Fatal("stopped doorbell camera must show a disabled action with a recovery hint")
	}
}

// Camera.Status cannot know whether a camera is administratively stopped -
// only the manager holds the running set. A server that walks the cameras
// itself therefore reports stopped=false for every camera, forever, and the
// dashboard shows a stopped camera as merely OFFLINE.
func TestListCamerasReportsStopped(t *testing.T) {
	srv, _ := newTestServer(t)
	cam := camera.NewTestCamera("front")
	cam.SetTestOnline(false)
	srv.cameras.RegisterForTest(cam)

	if got := listCameraState(t, srv, "front")["stopped"]; got != false {
		t.Errorf("stopped = %v for a running camera, want false", got)
	}

	if err := srv.cameras.StopCamera("front"); err != nil {
		t.Fatalf("StopCamera: %v", err)
	}

	if got := listCameraState(t, srv, "front")["stopped"]; got != true {
		t.Errorf("stopped = %v after StopCamera, want true", got)
	}
}

// An on-demand battery camera is down between events by design. The API has to
// say so, or every consumer reads the same online=false a broken camera
// reports and either alerts on it or trains the operator to ignore the alert.
func TestListCamerasReportsSleeping(t *testing.T) {
	srv, _ := newTestServer(t)

	mains := camera.NewTestCamera("wired")
	mains.SetTestOnline(false)
	srv.cameras.RegisterForTest(mains)

	battery := camera.NewTestCamera("battery")
	battery.SetTestOnDemand(true)
	battery.SetTestOnline(false)
	srv.cameras.RegisterForTest(battery)

	if got := listCameraState(t, srv, "wired")["sleeping"]; got != false {
		t.Errorf("sleeping = %v for a down mains camera, want false: that is a genuine outage", got)
	}
	if got := listCameraState(t, srv, "battery")["sleeping"]; got != true {
		t.Errorf("sleeping = %v for a resting on-demand camera, want true", got)
	}

	// Mid-event the camera is genuinely streaming, so it is not sleeping.
	battery.SetTestOnline(true)
	if got := listCameraState(t, srv, "battery")["sleeping"]; got != false {
		t.Errorf("sleeping = %v while the on-demand camera is streaming, want false", got)
	}
}

// Stopped and sleeping must not both be true: an operator turning a battery
// camera off is not the same fact as that camera resting between events, and a
// UI that saw both would promise it wakes on motion when it will not.
func TestListCamerasStoppedBeatsSleeping(t *testing.T) {
	srv, _ := newTestServer(t)
	cam := camera.NewTestCamera("battery")
	cam.SetTestOnDemand(true)
	cam.SetTestOnline(false)
	srv.cameras.RegisterForTest(cam)

	if err := srv.cameras.StopCamera("battery"); err != nil {
		t.Fatalf("StopCamera: %v", err)
	}

	state := listCameraState(t, srv, "battery")
	if state["stopped"] != true {
		t.Errorf("stopped = %v, want true", state["stopped"])
	}
	if state["sleeping"] != false {
		t.Errorf("sleeping = %v for a stopped camera, want false", state["sleeping"])
	}
}

// The grid partial is what the browser renders before any JS poll runs, so its
// badge has to agree with the JSON the poll will deliver a second later.
func TestCameraGridPartialBadges(t *testing.T) {
	srv, _ := newTestServer(t)

	live := camera.NewTestCamera("live-cam")
	live.SetTestOnline(true)
	srv.cameras.RegisterForTest(live)

	down := camera.NewTestCamera("down-cam")
	down.SetTestOnline(false)
	srv.cameras.RegisterForTest(down)

	asleep := camera.NewTestCamera("sleepy-cam")
	asleep.SetTestOnDemand(true)
	asleep.SetTestOnline(false)
	srv.cameras.RegisterForTest(asleep)

	off := camera.NewTestCamera("off-cam")
	off.SetTestOnline(false)
	srv.cameras.RegisterForTest(off)
	if err := srv.cameras.StopCamera("off-cam"); err != nil {
		t.Fatalf("StopCamera: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/partials/camera-grid", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()

	cards := map[string]string{}
	for _, chunk := range strings.Split(body, `data-camera-name="`)[1:] {
		name, rest, ok := strings.Cut(chunk, `"`)
		if !ok {
			continue
		}
		cards[name] = rest
	}

	for _, tc := range []struct {
		camera string
		dot    string
		label  string
	}{
		{"live-cam", `<span class="cam-live-dot"></span>`, "LIVE"},
		{"down-cam", `<span class="cam-live-dot offline"></span>`, "OFFLINE"},
		{"sleepy-cam", `<span class="cam-live-dot sleeping"></span>`, "SLEEPING"},
		{"off-cam", `<span class="cam-live-dot stopped"></span>`, "STOPPED"},
	} {
		card, ok := cards[tc.camera]
		if !ok {
			t.Errorf("camera %q missing from grid partial", tc.camera)
			continue
		}
		// Bound the badge on both sides. Containment against the whole card
		// would pass on markup that put the dot anywhere in the tile.
		_, badge, found := strings.Cut(card, `<div class="cam-live-badge">`)
		if !found {
			t.Errorf("%s: no live badge in card", tc.camera)
			continue
		}
		badge, _, _ = strings.Cut(badge, "</div>")
		if !strings.Contains(badge, tc.dot) {
			t.Errorf("%s: badge dot = %q, want it to contain %q", tc.camera, badge, tc.dot)
		}
		if !strings.Contains(badge, tc.label) {
			t.Errorf("%s: badge label = %q, want it to contain %q", tc.camera, badge, tc.label)
		}
	}
}
