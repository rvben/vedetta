package config

import (
	"errors"
	"strings"
	"testing"
)

const dupAuthSection = `auth:
  users:
    - username: admin
      password_hash: "$2a$10$hash"
`

// TestLoadRejectsDuplicateCameraNames covers the case where one physical camera
// entry is pasted twice: the manager keys its camera map by name, so the second
// entry replaces the first while the start order still lists the name twice,
// starting one camera object twice and leaking the first cancel func.
func TestLoadRejectsDuplicateCameraNames(t *testing.T) {
	path := writeTempConfig(t, dupAuthSection+`cameras:
  - name: front_door
    url: rtsp://192.0.2.10/stream
  - name: front_door
    url: rtsp://192.0.2.11/stream
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() accepted two cameras named front_door")
	}
	if !strings.Contains(err.Error(), "front_door") {
		t.Errorf("error must name the duplicate camera, got: %v", err)
	}
}

// TestLoadRejectsCaseInsensitiveDuplicateCameraNames covers names that differ
// only in case. They are distinct map keys but the same recording and snapshot
// directory on a case-insensitive filesystem, so the two cameras would overwrite
// each other's media.
func TestLoadRejectsCaseInsensitiveDuplicateCameraNames(t *testing.T) {
	path := writeTempConfig(t, dupAuthSection+`cameras:
  - name: Garage
    url: rtsp://192.0.2.10/stream
  - name: garage
    url: rtsp://192.0.2.11/stream
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() accepted cameras named Garage and garage")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "garage") {
		t.Errorf("error must name the duplicate camera, got: %v", err)
	}
}

// TestLoadAcceptsDistinctCameraNames guards against the duplicate check
// rejecting a valid config.
func TestLoadAcceptsDistinctCameraNames(t *testing.T) {
	path := writeTempConfig(t, dupAuthSection+`cameras:
  - name: front_door
    url: rtsp://192.0.2.10/stream
  - name: garage
    url: rtsp://192.0.2.11/stream
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() rejected distinct camera names: %v", err)
	}
	if len(cfg.Cameras) != 2 {
		t.Fatalf("got %d cameras, want 2", len(cfg.Cameras))
	}
}

// TestAppendCameraRejectsDuplicateName covers the add-camera path: the UI can
// submit a name that is already configured, which Load would then refuse on the
// next start, leaving a config file the server cannot read.
func TestAppendCameraRejectsDuplicateName(t *testing.T) {
	path := writeTempConfig(t, dupAuthSection+`cameras:
  - name: front_door
    url: rtsp://192.0.2.10/stream
`)

	err := AppendCamera(path, CameraConfig{Name: "front_door", URL: "rtsp://192.0.2.11/stream"}, "")
	if err == nil {
		t.Fatal("AppendCamera() accepted a name that is already configured")
	}
	if !strings.Contains(err.Error(), "front_door") {
		t.Errorf("error must name the duplicate camera, got: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() after rejected append: %v", err)
	}
	if len(cfg.Cameras) != 1 {
		t.Fatalf("got %d cameras after rejected append, want 1", len(cfg.Cameras))
	}
}

// TestAppendCameraRejectsCaseInsensitiveDuplicateName mirrors the Load rule so
// the two validators cannot disagree about what a duplicate is.
func TestAppendCameraRejectsCaseInsensitiveDuplicateName(t *testing.T) {
	path := writeTempConfig(t, dupAuthSection+`cameras:
  - name: garage
    url: rtsp://192.0.2.10/stream
`)

	if err := AppendCamera(path, CameraConfig{Name: "Garage", URL: "rtsp://192.0.2.11/stream"}, ""); err == nil {
		t.Fatal("AppendCamera() accepted Garage next to garage")
	}
}

// TestAppendCameraAcceptsNewName guards the accepting case.
func TestAppendCameraAcceptsNewName(t *testing.T) {
	path := writeTempConfig(t, dupAuthSection+`cameras:
  - name: front_door
    url: rtsp://192.0.2.10/stream
`)

	if err := AppendCamera(path, CameraConfig{Name: "garage", URL: "rtsp://192.0.2.11/stream"}, ""); err != nil {
		t.Fatalf("AppendCamera() rejected a new name: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if len(cfg.Cameras) != 2 {
		t.Fatalf("got %d cameras, want 2", len(cfg.Cameras))
	}
}

// TestUpdateCameraRejectsDuplicateName covers the rename path. The UI edits a
// camera by index, so a rename onto a name another entry already holds writes a
// file that Load then refuses, and the server does not start again until
// somebody edits the YAML by hand.
func TestUpdateCameraRejectsDuplicateName(t *testing.T) {
	path := writeTempConfig(t, dupAuthSection+`cameras:
  - name: front_door
    url: rtsp://192.0.2.10/stream
  - name: garage
    url: rtsp://192.0.2.11/stream
`)

	err := UpdateCamera(path, 1, CameraConfig{Name: "front_door", URL: "rtsp://192.0.2.11/stream"})
	if err == nil {
		t.Fatal("UpdateCamera() renamed garage onto front_door")
	}
	if !errors.Is(err, ErrDuplicateCameraName) {
		t.Errorf("error must be ErrDuplicateCameraName so the API can answer 409, got: %v", err)
	}
	if !strings.Contains(err.Error(), "front_door") {
		t.Errorf("error must name the duplicate camera, got: %v", err)
	}

	// The invariant that matters is not the error, it is that a refused write
	// leaves a file the next start can still read.
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() after rejected rename: %v", err)
	}
	if len(cfg.Cameras) != 2 || cfg.Cameras[1].Name != "garage" {
		t.Fatalf("rejected rename changed the file: %+v", cfg.Cameras)
	}
}

// TestUpdateCameraRejectsCaseInsensitiveDuplicateName keeps the rename rule
// identical to the one Load and AppendCamera apply.
func TestUpdateCameraRejectsCaseInsensitiveDuplicateName(t *testing.T) {
	path := writeTempConfig(t, dupAuthSection+`cameras:
  - name: front_door
    url: rtsp://192.0.2.10/stream
  - name: garage
    url: rtsp://192.0.2.11/stream
`)

	if err := UpdateCamera(path, 1, CameraConfig{Name: "Front_Door", URL: "rtsp://192.0.2.11/stream"}); err == nil {
		t.Fatal("UpdateCamera() renamed garage onto Front_Door")
	}
}

// TestUpdateCameraAcceptsItsOwnName guards the check against rejecting an
// ordinary edit: changing a camera's URL leaves its name in place, and that
// name must not count as a duplicate of itself.
func TestUpdateCameraAcceptsItsOwnName(t *testing.T) {
	path := writeTempConfig(t, dupAuthSection+`cameras:
  - name: front_door
    url: rtsp://192.0.2.10/stream
  - name: garage
    url: rtsp://192.0.2.11/stream
`)

	if err := UpdateCamera(path, 0, CameraConfig{Name: "front_door", URL: "rtsp://192.0.2.99/stream"}); err != nil {
		t.Fatalf("UpdateCamera() rejected a camera keeping its own name: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.Cameras[0].URL != "rtsp://192.0.2.99/stream" {
		t.Fatalf("edit was not written: %q", cfg.Cameras[0].URL)
	}
}

// TestUpdateCameraAcceptsFreeName guards the accepting case for a real rename.
func TestUpdateCameraAcceptsFreeName(t *testing.T) {
	path := writeTempConfig(t, dupAuthSection+`cameras:
  - name: front_door
    url: rtsp://192.0.2.10/stream
  - name: garage
    url: rtsp://192.0.2.11/stream
`)

	if err := UpdateCamera(path, 1, CameraConfig{Name: "driveway", URL: "rtsp://192.0.2.11/stream"}); err != nil {
		t.Fatalf("UpdateCamera() rejected a free name: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.Cameras[1].Name != "driveway" {
		t.Fatalf("rename was not written: %q", cfg.Cameras[1].Name)
	}
}

// TestUpdateCameraRejectsInvalidName covers the other rule that would produce a
// file Load refuses: a name with a path separator in it.
func TestUpdateCameraRejectsInvalidName(t *testing.T) {
	path := writeTempConfig(t, dupAuthSection+`cameras:
  - name: front_door
    url: rtsp://192.0.2.10/stream
`)

	if err := UpdateCamera(path, 0, CameraConfig{Name: "../escape", URL: "rtsp://192.0.2.10/stream"}); err == nil {
		t.Fatal("UpdateCamera() accepted a name with a path traversal in it")
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load() after rejected rename: %v", err)
	}
}
