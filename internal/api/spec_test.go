package api

import (
	"os"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestOpenAPISpecIsValid(t *testing.T) {
	data, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}

	loader := openapi3.NewLoader()
	spec, err := loader.LoadFromData(data)
	if err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}

	if err := spec.Validate(loader.Context); err != nil {
		t.Fatalf("validate openapi.yaml: %v", err)
	}

	expectedPaths := []string{
		// Discovery
		"/api/openapi.json",

		// Health
		"/api/health",
		"/api/health/live",
		"/api/health/ready",

		// System
		"/api/system",
		"/api/system/recompress/trigger",
		"/metrics",

		// Auth
		"/api/auth/login",
		"/api/auth/logout",
		"/api/auth/me",
		"/api/auth/change-password",

		// Tokens
		"/api/tokens",
		"/api/tokens/{id}",

		// Cameras
		"/api/cameras",
		"/api/cameras/{name}",
		"/api/cameras/{name}/snapshot",
		"/api/cameras/{name}/thumbnail",
		"/api/cameras/{name}/ptz",
		"/api/cameras/{name}/doorbell",
		"/api/cameras/{name}/stop",
		"/api/cameras/{name}/start",

		// Zones
		"/api/cameras/{name}/zones",
		"/api/cameras/{name}/zones/{zone}",
		"/api/cameras/{name}/zones/{zone}/presence",

		// Events
		"/api/activities",
		"/api/activities/counts",
		"/api/activities/{id}",
		"/api/events",
		"/api/events/{id}",
		"/api/events/{id}/snapshot",
		"/api/events/{id}/clip",
		"/api/events/{id}/detection-crop",
		"/api/events/counts",
		"/api/events/stream",
		"/api/events/{id}/identify",
		"/api/events/{id}/track-person",
		"/api/events/{id}/assign-person",

		// Recordings
		"/api/recordings/segments/{camera}",
		"/api/recordings/calendar",
		"/api/recordings/summary",
		"/api/recordings/export/{camera}",

		// Playback / HLS
		"/api/cameras/{name}/timeline",
		"/api/cameras/{name}/playback.m3u8",
		"/api/cameras/{name}/segments/{id}",
		"/api/cameras/{name}/segments/{id}/hls/init.mp4",
		"/api/cameras/{name}/segments/{id}/hls/{segNum}",

		// People & Faces
		"/api/people",
		"/api/people/{id}",
		"/api/people/{id}/faces",
		"/api/people/{id}/events",
		"/api/people/merge",
		"/api/faces/unmatched",
		"/api/faces/{id}/assign",
		"/api/faces/{id}/crop",
		"/api/faces/{id}/ignore",
		"/api/faces/backfill",

		// Objects
		"/api/objects",
		"/api/objects/{id}",
		"/api/objects/{id}/sightings",
		"/api/objects/{id}/crop",
		"/api/objects/{id}/references",
		"/api/objects/references/{id}",
		"/api/objects/sightings/{id}",
		"/api/cameras/{name}/objects",

		// Streaming
		"/api/cameras/{name}/webrtc/offer",
		"/api/cameras/{name}/mse/ws",
		"/api/cameras/{name}/mjpeg",
	}
	for _, p := range expectedPaths {
		if spec.Paths.Find(p) == nil {
			t.Errorf("expected path %s not found in spec", p)
		}
	}
}
