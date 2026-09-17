package notify

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/storage"
)

func testPushover(t *testing.T) *Pushover {
	t.Helper()
	dir := t.TempDir()
	app, user := filepath.Join(dir, "app"), filepath.Join(dir, "user")
	for path, value := range map[string]string{app: strings.Repeat("a", 30), user: strings.Repeat("u", 30)} {
		if err := os.WriteFile(path, []byte(value+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	p, err := NewPushover(config.PushoverConfig{
		AppTokenFile: app, UserKeyFile: user,
		BaseURL: "https://vedetta.example.com", Cameras: []string{"front_door"},
	})
	if err != nil {
		t.Fatal(err)
	}
	p.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return p
}

func pushoverRing() camera.Event {
	return camera.Event{ID: "ring-1", CameraName: "front_door", Kind: camera.EventKindDoorbell,
		Label: "doorbell", Timestamp: time.Unix(1_800_000_000, 0)}
}

func TestPushoverPhotoDelivery(t *testing.T) {
	p := testPushover(t)
	p.cfg.Device, p.cfg.Sound, p.cfg.Priority = "phone", "incoming", 1
	ev := pushoverRing()
	ev.SnapshotPath = filepath.Join(t.TempDir(), "ring.jpg")
	ev.SnapshotAvailable = true
	img := image.NewRGBA(image.Rect(0, 0, 2560, 1920))
	for y := range 1920 {
		for x := range 2560 {
			img.SetRGBA(x, y, color.RGBA{R: 220, A: 255})
		}
	}
	var original bytes.Buffer
	if err := jpeg.Encode(&original, img, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ev.SnapshotPath, original.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.Header.Get("User-Agent") != "Vedetta" {
			t.Errorf("unexpected request method or identity")
		}
		if err := r.ParseMultipartForm(6 << 20); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		defer r.MultipartForm.RemoveAll()
		for key, want := range map[string]string{
			"token": strings.Repeat("a", 30), "user": strings.Repeat("u", 30),
			"title": "Someone's at the door", "device": "phone", "sound": "incoming", "priority": "1",
			"timestamp": "1800000000", "url": "https://vedetta.example.com/doorbell-answer.html?camera=front_door&event=ring-1",
		} {
			if r.FormValue(key) != want {
				t.Errorf("incorrect field %s", key)
			}
		}
		f, header, err := r.FormFile("attachment")
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		if header.Header.Get("Content-Type") != "image/jpeg" || header.Size > 5_242_880 {
			t.Error("invalid photo MIME type or size")
		}
		photo, err := jpeg.Decode(f)
		if err != nil {
			t.Error(err)
			return
		}
		if photo.Bounds().Dx() != 1280 || photo.Bounds().Dy() != 960 {
			t.Errorf("photo dimensions: %v", photo.Bounds())
		}
		red, _, blue, _ := photo.At(640, 480).RGBA()
		if red < 40_000 || blue > 5_000 {
			t.Error("attachment does not match the event photo")
		}
		fmt.Fprint(w, `{"status":1}`)
	}))
	defer srv.Close()
	p.endpoint = srv.URL
	if !p.EnqueueDoorbell(ev) {
		t.Fatal("ring not queued")
	}
	if err := p.send(context.Background(), <-p.jobs); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatal("expected one notification")
	}
}

func TestPushoverWithoutPhotoAndRejectedResponses(t *testing.T) {
	for _, response := range []struct {
		name string
		code int
		body string
		ok   bool
	}{
		{"accepted", 200, `{"status":1}`, true},
		{"rejected", 400, `{"status":0,"errors":["secret-credential"]}`, false},
		{"false_success", 200, `{"status":0}`, false},
		{"malformed", 200, `not-json`, false},
		{"rate_limited", 429, `{"status":0}`, false},
	} {
		t.Run(response.name, func(t *testing.T) {
			p := testPushover(t)
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Error(err)
					return
				}
				defer r.MultipartForm.RemoveAll()
				if len(r.MultipartForm.File) != 0 || r.FormValue("message") == "" {
					t.Error("missing photo must still send a text notification")
				}
				w.WriteHeader(response.code)
				fmt.Fprint(w, response.body)
			}))
			defer srv.Close()
			p.endpoint = srv.URL
			ev := pushoverRing()
			ev.SnapshotAvailable, ev.SnapshotPath = true, filepath.Join(t.TempDir(), "missing.jpg")
			p.EnqueueDoorbell(ev)
			err := p.send(context.Background(), <-p.jobs)
			if (err == nil) != response.ok {
				t.Fatalf("send error = %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "secret-credential") {
				t.Fatal("provider response leaked credentials")
			}
			if requests.Load() != 1 {
				t.Fatal("must not retry a send")
			}
		})
	}
}

func TestPushoverDoesNotFollowRedirects(t *testing.T) {
	p := testPushover(t)
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	p.endpoint = source.URL
	p.EnqueueDoorbell(pushoverRing())
	if err := p.send(context.Background(), <-p.jobs); err == nil || leaked.Load() {
		t.Fatal("redirect must fail without forwarding credentials")
	}
}

func TestPushoverHonorsDeliveryDeadline(t *testing.T) {
	p := testPushover(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
	}))
	defer srv.Close()
	defer close(release)
	p.endpoint = srv.URL
	p.EnqueueDoorbell(pushoverRing())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- p.send(ctx, <-p.jobs) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not reach mock provider")
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("cancelled send reported success")
		}
	case <-time.After(time.Second):
		t.Fatal("provider ignored cancellation")
	}
}

func TestPushoverFiltersAndDoorbellDeduplication(t *testing.T) {
	p := testPushover(t)
	ring := pushoverRing()
	other := ring
	other.CameraName = "garage"
	if !p.EnqueueDoorbell(other) || len(p.jobs) != 0 {
		t.Fatal("camera filter not respected")
	}
	if !p.EnqueueDoorbell(ring) || len(p.jobs) != 1 {
		t.Fatal("ring not queued immediately")
	}
	a := storage.Activity{ID: "activity-1", CameraName: "front_door", HasDoorbell: true, PrimaryEvent: ring}
	if !p.EnqueueActivity(a) || len(p.jobs) != 1 {
		t.Fatal("finalized ring activity must not send a duplicate")
	}
	a.HasDoorbell = false
	if !p.EnqueueActivity(a) || len(p.jobs) != 2 {
		t.Fatal("ordinary alert activity not queued")
	}
	p.cfg.Events = []string{"doorbell"}
	if !p.EnqueueActivity(a) || len(p.jobs) != 2 {
		t.Fatal("doorbell-only destination sent detection alerts")
	}
	p.cfg.Events = []string{"activity"}
	if !p.EnqueueDoorbell(ring) || len(p.jobs) != 2 {
		t.Fatal("activity-only destination sent an immediate ring")
	}
	a.HasDoorbell = true
	if !p.EnqueueActivity(a) || len(p.jobs) != 3 {
		t.Fatal("activity-only destination lost the doorbell incident")
	}
	a.Category = camera.CategoryDetection
	if !p.EnqueueActivity(a) || len(p.jobs) != 3 {
		t.Fatal("low-priority detection sent a push")
	}
}

func TestPushoverQueueBoundAndCancellation(t *testing.T) {
	p := testPushover(t)
	for range cap(p.jobs) {
		if !p.EnqueueDoorbell(pushoverRing()) {
			t.Fatal("queue filled early")
		}
	}
	if p.EnqueueDoorbell(pushoverRing()) {
		t.Fatal("full queue must refuse immediately")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.Start(ctx)
	done := make(chan struct{})
	go func() { p.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("workers did not stop on cancellation")
	}
}

func TestPushoverInvalidCredentialsAreRedacted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("secret-invalid-token"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := NewPushover(config.PushoverConfig{AppTokenFile: path, UserKeyFile: path})
	if err == nil || strings.Contains(err.Error(), "secret-invalid-token") {
		t.Fatalf("credential error must be redacted: %v", err)
	}
}
