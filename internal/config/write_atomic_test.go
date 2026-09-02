package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

const atomicBaseConfig = `auth:
  users:
    - username: admin
      password_hash: "$2a$10$hash"
mqtt:
  enabled: false
  host: initial
  port: 1883
recording:
  path: ./recordings
  segment_length: 10m
  pre_capture: 5s
  post_capture: 10s
  retain_days: 7
  event_retain_days: 30
  max_storage: 1GB
`

// TestConcurrentSectionUpdatesKeepBothWrites drives two settings updates at once,
// as two browser tabs or the UI and an automation would. Each update is a read,
// modify and write of the whole document, so without a lock the second writer
// bases its document on a snapshot taken before the first writer's change and
// silently restores the old value of the other section.
func TestConcurrentSectionUpdatesKeepBothWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(atomicBaseConfig), 0600); err != nil {
		t.Fatal(err)
	}

	const rounds = 40
	var wg sync.WaitGroup
	errs := make(chan error, 2*rounds)

	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			if err := UpdateMQTT(path, MQTTConfig{Enabled: true, Host: "broker", Port: 1883 + i}); err != nil {
				errs <- err
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			rec := Defaults().Recording
			rec.RetainDays = 100 + i
			if err := UpdateRecording(path, rec); err != nil {
				errs <- err
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent update failed: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("config unreadable after concurrent updates: %v", err)
	}
	if cfg.MQTT.Port != 1883+rounds-1 {
		t.Errorf("mqtt.port = %d, want %d: an mqtt write was lost", cfg.MQTT.Port, 1883+rounds-1)
	}
	if cfg.Recording.RetainDays != 100+rounds-1 {
		t.Errorf("recording.retain_days = %d, want %d: a recording write was lost", cfg.Recording.RetainDays, 100+rounds-1)
	}
}

// TestConfigWriteNeverExposesPartialDocument reads the config file while it is
// being rewritten. A truncate-then-write leaves a window in which a reader (the
// next start, or another settings update) sees an empty or half-written
// document; replacing the file by rename has no such window.
func TestConfigWriteNeverExposesPartialDocument(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")

	// Padding makes the write large enough that a torn read is observable. The
	// comment is preserved by the yaml.Node round trip, so every rewrite is the
	// same size.
	var padding strings.Builder
	for i := 0; i < 20000; i++ {
		padding.WriteString("# padding line to widen the write window\n")
	}
	if err := os.WriteFile(path, []byte(padding.String()+atomicBaseConfig), 0600); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	torn := make(chan string, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var doc map[string]any
			if err := yaml.Unmarshal(data, &doc); err != nil {
				select {
				case torn <- "unparseable document: " + err.Error():
				default:
				}
				return
			}
			if _, ok := doc["auth"]; !ok {
				select {
				case torn <- "document missing the auth section":
				default:
				}
				return
			}
		}
	}()

	for i := 0; i < 30; i++ {
		if err := UpdateMQTT(path, MQTTConfig{Enabled: true, Host: "broker", Port: 1883 + i}); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("UpdateMQTT: %v", err)
		}
	}
	close(stop)
	wg.Wait()

	select {
	case reason := <-torn:
		t.Fatalf("reader observed a partially written config: %s", reason)
	default:
	}
}

// TestFailedUpdateLeavesConfigIntact makes the write fail after the document has
// been encoded. The previous config must survive untouched: it holds every
// camera URL and credential, and the parser is strict, so a truncated file
// leaves the next start with no cameras.
func TestFailedUpdateLeavesConfigIntact(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root ignores directory permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(atomicBaseConfig), 0600); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// A directory without write permission blocks creating the replacement file
	// while still allowing an in-place truncating write of the existing file.
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })

	if err := UpdateMQTT(path, MQTTConfig{Enabled: true, Host: "broker", Port: 1884}); err == nil {
		t.Fatal("UpdateMQTT must report the failure instead of reporting success")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("config unreadable after a failed update: %v", err)
	}
	if string(after) != string(original) {
		t.Errorf("failed update modified the config file:\n--- before ---\n%s\n--- after ---\n%s", original, after)
	}
}

// TestUpdatePreservesFileMode keeps the config's permissions across a rewrite:
// it holds camera credentials, so widening the mode would expose them.
func TestUpdatePreservesFileMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(atomicBaseConfig), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}

	if err := UpdateMQTT(path, MQTTConfig{Enabled: true, Host: "broker", Port: 1884}); err != nil {
		t.Fatalf("UpdateMQTT: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0640 {
		t.Errorf("file mode = %o after update, want 640", perm)
	}
}

// TestAppendCameraPreservesFileMode covers the second write path, which stats
// the file for its mode before writing.
func TestAppendCameraPreservesFileMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(atomicBaseConfig), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}

	if err := AppendCamera(path, CameraConfig{Name: "garage", URL: "rtsp://192.0.2.10/stream"}, ""); err != nil {
		t.Fatalf("AppendCamera: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0640 {
		t.Errorf("file mode = %o after append, want 640", perm)
	}
}
