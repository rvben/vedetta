package notify

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/storage"
)

func newTestSigner(t *testing.T) *SnapshotSigner {
	t.Helper()
	signer, err := LoadOrGenerateSnapshotSigner(newFakeKVStore())
	if err != nil {
		t.Fatalf("LoadOrGenerateSnapshotSigner: %v", err)
	}
	return signer
}

func TestBuildPayload_WithSnapshot(t *testing.T) {
	ev := camera.Event{
		ID:                "front-t91-1712847123456",
		CameraName:        "front_door",
		Label:             "person",
		Score:             0.87,
		Timestamp:         time.Date(2026, 4, 11, 18, 42, 0, 0, time.UTC),
		SnapshotAvailable: true,
	}
	signer := newTestSigner(t)
	data := BuildPayload(ev, signer)

	var got map[string]interface{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Title should be the friendly form of the camera name — underscores
	// become spaces, each word title-cased. "front_door" → "Front Door".
	if got["title"] != "Front Door" {
		t.Errorf("title = %v, want %q", got["title"], "Front Door")
	}
	if !strings.Contains(got["body"].(string), "Person") || !strings.Contains(got["body"].(string), "18:42 UTC") {
		t.Errorf("body = %v", got["body"])
	}
	if got["url"] != "/event.html?id=front-t91-1712847123456" {
		t.Errorf("url = %v", got["url"])
	}
	image, _ := got["image"].(string)
	if !strings.HasPrefix(image, "/api/push/snapshot/front-t91-1712847123456?") {
		t.Errorf("image = %v, expected signed /api/push/snapshot/ URL", got["image"])
	}
	// The signed URL must carry both an expiry and a signature so the
	// handler can validate it without any session state.
	if !strings.Contains(image, "e=") || !strings.Contains(image, "s=") {
		t.Errorf("image URL missing e= or s= params: %v", image)
	}
	// Tag is a deduplication key used by showNotification() — keep the
	// raw camera name so distinct cameras never collide in the OS
	// notification stack, regardless of the friendly title.
	if got["tag"] != "front_door:person" {
		t.Errorf("tag = %v", got["tag"])
	}
}

func TestBuildPayload_OmitsImageWhenUnavailable(t *testing.T) {
	ev := camera.Event{
		ID:                "front-t91",
		CameraName:        "front",
		Label:             "person",
		Timestamp:         time.Now().UTC(),
		SnapshotAvailable: false,
	}
	signer := newTestSigner(t)
	data := BuildPayload(ev, signer)
	var got map[string]interface{}
	_ = json.Unmarshal(data, &got)
	if _, has := got["image"]; has {
		t.Fatalf("image should be omitted when SnapshotAvailable is false, payload: %s", string(data))
	}
}

func TestBuildPayload_OmitsImageWhenSignerNil(t *testing.T) {
	ev := camera.Event{
		ID:                "front-t91",
		CameraName:        "front",
		Label:             "person",
		Timestamp:         time.Now().UTC(),
		SnapshotAvailable: true,
	}
	data := BuildPayload(ev, nil)
	var got map[string]interface{}
	_ = json.Unmarshal(data, &got)
	if _, has := got["image"]; has {
		t.Fatalf("image should be omitted when signer is nil, payload: %s", string(data))
	}
}

func TestBuildPayload_FitsUnder4KB(t *testing.T) {
	ev := camera.Event{
		ID:                strings.Repeat("x", 200),
		CameraName:        strings.Repeat("c", 100),
		Label:             strings.Repeat("l", 100),
		Timestamp:         time.Now().UTC(),
		SnapshotAvailable: true,
	}
	data := BuildPayload(ev, newTestSigner(t))
	if len(data) > 4000 {
		t.Fatalf("payload too large: %d bytes", len(data))
	}
}

func TestBuildActivityPayload(t *testing.T) {
	start := time.Date(2026, 8, 24, 18, 42, 0, 0, time.UTC)
	activity := storage.Activity{
		ID:           "act_front-1",
		CameraName:   "front_door",
		StartTime:    start,
		EventCount:   2,
		Labels:       []string{"car", "person"},
		PrimaryEvent: camera.Event{ID: "front-1", SnapshotAvailable: true},
	}
	data := BuildActivityPayload(activity, newTestSigner(t))
	var got map[string]interface{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["title"] != "Front Door" || got["url"] != "/activity.html?id=act_front-1" {
		t.Fatalf("activity payload = %s", data)
	}
	if got["tag"] != "activity:act_front-1" {
		t.Errorf("tag = %v", got["tag"])
	}
	if body, _ := got["body"].(string); !strings.Contains(body, "Car, Person · 2 events · 18:42 UTC") {
		t.Errorf("body = %q", body)
	}
	if image, _ := got["image"].(string); !strings.HasPrefix(image, "/api/push/snapshot/front-1?") {
		t.Errorf("image = %q", image)
	}
}

func TestBuildTestPayloadUsesSignedSnapshotWhenAvailable(t *testing.T) {
	at := time.Date(2026, 8, 24, 18, 42, 0, 0, time.UTC)
	data := BuildTestPayload("front_door", "front-latest", at, newTestSigner(t))
	var got map[string]interface{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["title"] != "Vedetta notification test" || got["url"] != "/settings.html" {
		t.Fatalf("test payload = %s", data)
	}
	if body, _ := got["body"].(string); body != "Notifications are working · Front Door" {
		t.Errorf("body = %q", body)
	}
	if image, _ := got["image"].(string); !strings.HasPrefix(image, "/api/push/snapshot/front-latest?") {
		t.Errorf("image = %q", image)
	}
}

func TestBuildTestPayloadFallsBackWhenSnapshotUnavailable(t *testing.T) {
	data := BuildTestPayload("front_door", "", time.Now(), newTestSigner(t))
	var got map[string]interface{}
	_ = json.Unmarshal(data, &got)
	if _, exists := got["image"]; exists {
		t.Fatalf("image should be omitted without a snapshot event: %s", data)
	}
}

func TestTitleCase(t *testing.T) {
	if titleCase("person") != "Person" {
		t.Fatalf("expected 'Person', got %q", titleCase("person"))
	}
	if titleCase("") != "" {
		t.Fatalf("expected empty, got %q", titleCase(""))
	}
}

func TestFriendlyCameraName(t *testing.T) {
	cases := map[string]string{
		"front_door":     "Front Door",
		"kids_bedroom_3": "Kids Bedroom 3",
		"garage":         "Garage",
		"driveway-east":  "Driveway East",
		"":               "",
		"A":              "A",
		"back_YARD":      "Back YARD", // uppercase preserved, only first byte touched
	}
	for in, want := range cases {
		if got := friendlyCameraName(in); got != want {
			t.Errorf("friendlyCameraName(%q) = %q, want %q", in, got, want)
		}
	}
}
