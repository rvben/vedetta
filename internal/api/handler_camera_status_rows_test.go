package api

import (
	"strings"
	"testing"

	"github.com/rvben/vedetta/internal/camera"
)

// A camera that is resting or switched off is not a fault, and the Settings
// card must not call it Offline.
func TestCameraStatusPresentation(t *testing.T) {
	cases := []struct {
		name      string
		status    camera.CameraStatus
		wantLabel string
		wantClass string
	}{
		{"online", camera.CameraStatus{Online: true}, "Online", "badge-connected"},
		{"degraded", camera.CameraStatus{Online: true, Degraded: true}, "Degraded", "badge-degraded"},
		{"offline", camera.CameraStatus{}, "Offline", "badge-disconnected"},
		{"sleeping", camera.CameraStatus{Sleeping: true}, "Sleeping", "badge-connecting"},
		{"stopped", camera.CameraStatus{Stopped: true}, "Stopped", "badge-connecting"},
		{"stopped wins over sleeping", camera.CameraStatus{Stopped: true, Sleeping: true}, "Stopped", "badge-connecting"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			label, class := cameraStatusPresentation(tc.status)
			if label != tc.wantLabel || class != tc.wantClass {
				t.Errorf("got (%q, %q), want (%q, %q)", label, class, tc.wantLabel, tc.wantClass)
			}
		})
	}
}

// The card uses the same row layout as its neighbours, with a styled state
// badge, and no bare table.
func TestSystemPartialCameraStatusRows(t *testing.T) {
	srv, _ := newTestServer(t)
	cam := camera.NewTestCamera("front_door")
	cam.SetTestOnline(true)
	srv.cameras.RegisterForTest(cam)

	body := getBody(t, srv, "/partials/system")
	if strings.Contains(body, "<table") {
		t.Errorf("system partial renders a table:\n%s", body)
	}
	want := `<span class="key">Front Door</span><span class="val"><span class="badge-connected">Online</span></span>`
	if !strings.Contains(body, want) {
		t.Errorf("camera status row missing, want %s in:\n%s", want, body)
	}
}
