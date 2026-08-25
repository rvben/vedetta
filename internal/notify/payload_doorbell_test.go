package notify

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/storage"
)

func TestBuildPayload_Doorbell(t *testing.T) {
	ev := camera.Event{ID: "r1", CameraName: "front_door", Label: "doorbell", Kind: camera.EventKindDoorbell, Timestamp: time.Now()}
	var p map[string]any
	if err := json.Unmarshal(BuildPayload(ev, nil), &p); err != nil {
		t.Fatal(err)
	}
	if p["title"] != "Someone's at the door" {
		t.Errorf("title = %v, want \"Someone's at the door\"", p["title"])
	}
	if p["url"] != "/doorbell-answer.html?camera=front_door&event=r1" {
		t.Errorf("url = %v, want doorbell answer deep link", p["url"])
	}
	if p["tag"] != "front_door:doorbell" {
		t.Errorf("tag = %v, want replacement tag", p["tag"])
	}
}

func TestBuildActivityPayload_DoorbellReplacesImmediateRing(t *testing.T) {
	ev := camera.Event{ID: "r3", CameraName: "front_door", Label: "doorbell", Kind: camera.EventKindDoorbell, Timestamp: time.Now()}
	activity := storage.Activity{ID: "act-1", CameraName: ev.CameraName, StartTime: ev.Timestamp, EventCount: 1, HasDoorbell: true, PrimaryEvent: ev}
	var p map[string]any
	if err := json.Unmarshal(BuildActivityPayload(activity, nil), &p); err != nil {
		t.Fatal(err)
	}
	if p["tag"] != "front_door:doorbell" {
		t.Errorf("tag = %v, want immediate ring replacement tag", p["tag"])
	}
}

func TestBuildPayload_DoorbellWithPerson(t *testing.T) {
	ev := camera.Event{ID: "r2", CameraName: "front_door", Label: "doorbell", Kind: camera.EventKindDoorbell, SubLabel: "Alice", Timestamp: time.Now()}
	var p map[string]any
	if err := json.Unmarshal(BuildPayload(ev, nil), &p); err != nil {
		t.Fatal(err)
	}
	if p["title"] != "Alice is at the door" {
		t.Errorf("title = %v, want \"Alice is at the door\"", p["title"])
	}
}

func TestBuildPayload_ObjectUnchanged(t *testing.T) {
	ev := camera.Event{ID: "o1", CameraName: "front_yard", Label: "person", Kind: camera.EventKindObject, Timestamp: time.Now()}
	var p map[string]any
	if err := json.Unmarshal(BuildPayload(ev, nil), &p); err != nil {
		t.Fatal(err)
	}
	// Object events keep the existing friendly-camera-name title; must NOT be doorbell copy.
	if p["title"] == "Someone's at the door" {
		t.Error("object event got doorbell title")
	}
}
