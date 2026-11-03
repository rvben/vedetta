package notify

import (
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/snapshot"
)

func TestCryPayloadAndPushoverFilters(t *testing.T) {
	ev := camera.Event{ID: "cry-1", CameraName: "nursery", Kind: camera.EventKindCry, Label: "baby_cry", Timestamp: time.Now(), Category: camera.CategoryAlert}
	var payload pushPayload
	if err := json.Unmarshal(BuildPayload(ev, nil), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Title != "Crying heard" || payload.Tag != "nursery:cry" || payload.URL != "/camera.html?name=nursery" {
		t.Fatalf("wrong cry notification: %+v", payload)
	}
	p := testPushover(t)
	p.cfg.Cameras = nil
	if !p.EnqueueCry(ev) || len(p.jobs) != 1 {
		t.Fatal("cry alert not queued")
	}
	ev.Category = camera.CategoryDetection
	p.EnqueueCry(ev)
	if len(p.jobs) != 1 {
		t.Fatal("observation sent an alert")
	}
	ev.Category = camera.CategoryAlert
	p.cfg.Events = []string{"doorbell"}
	p.EnqueueCry(ev)
	if len(p.jobs) != 1 {
		t.Fatal("provider filter ignored")
	}
}

func TestPushoverCryPhotoAndWatchLiveAction(t *testing.T) {
	p := testPushover(t)
	p.cfg.Cameras = nil
	photo, err := snapshot.EncodeNotificationJPEG(image.NewRGBA(image.Rect(0, 0, 64, 48)))
	if err != nil {
		t.Fatal(err)
	}
	ev := camera.Event{ID: "cry-photo", CameraName: "nursery", Kind: camera.EventKindCry, Label: "baby_cry", Category: camera.CategoryAlert, Timestamp: time.Now(), NotificationJPEG: photo}
	if after := p.EnqueueCryEarly(ev); after != nil {
		t.Fatal("prepared photo did not enqueue early")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		defer r.MultipartForm.RemoveAll()
		for key, want := range map[string]string{"title": "Crying heard", "url_title": "Watch live", "url": "https://vedetta.example.com/camera.html?name=nursery"} {
			if r.FormValue(key) != want {
				t.Errorf("%s=%q, want %q", key, r.FormValue(key), want)
			}
		}
		file, _, err := r.FormFile("attachment")
		if err != nil {
			t.Error(err)
			return
		}
		defer file.Close()
		if _, err := jpeg.Decode(file); err != nil {
			t.Error(err)
		}
		_, _ = w.Write([]byte(`{"status":1}`))
	}))
	defer srv.Close()
	p.endpoint = srv.URL
	if err := p.send(context.Background(), <-p.jobs); err != nil {
		t.Fatal(err)
	}
}

type earlyCryTestProvider struct{ cryTestProvider }

func (p *earlyCryTestProvider) EnqueueCryEarly(ev camera.Event) func(camera.Event) bool {
	if p.EnqueueCry(ev) {
		return nil
	}
	return p.EnqueueCry
}

func TestCryEarlyFanoutRetriesOnlyRemainingProviders(t *testing.T) {
	early := &earlyCryTestProvider{cryTestProvider: cryTestProvider{testProvider: testProvider{accept: true}}}
	full := &cryTestProvider{}
	ready := &cryTestProvider{testProvider: testProvider{accept: true}}
	after := NewFanout(early, full, ready).EnqueueCryEarly(camera.Event{ID: "cry-early"})
	if after == nil || early.cries != 1 || full.cries != 0 || ready.cries != 0 {
		t.Fatal("incorrect early cry delivery")
	}
	if after(camera.Event{}) || early.cries != 1 || full.cries != 1 || ready.cries != 1 {
		t.Fatal("partial delivery duplicated or skipped a provider")
	}
	full.accept = true
	if !after(camera.Event{}) || early.cries != 1 || full.cries != 2 || ready.cries != 1 {
		t.Fatal("retry duplicated an accepted provider")
	}
}

type cryTestProvider struct {
	testProvider
	cries int
}

func (p *cryTestProvider) EnqueueCry(camera.Event) bool {
	p.cries++
	return p.accept
}

func TestCryFanoutRetryDoesNotDuplicateAcceptedProvider(t *testing.T) {
	full, ready := &cryTestProvider{}, &cryTestProvider{testProvider: testProvider{accept: true}}
	f := NewFanout(full, ready)
	ev := camera.Event{ID: "cry-retry", Kind: camera.EventKindCry, Category: camera.CategoryAlert}
	if f.EnqueueCry(ev) || full.cries != 1 || ready.cries != 1 {
		t.Fatal("cry should await the full provider")
	}
	full.accept = true
	if !f.EnqueueCry(ev) || full.cries != 2 || ready.cries != 1 {
		t.Fatal("retry duplicated an accepted cry notification")
	}
	if len(f.pending) != 0 {
		t.Fatal("completed cry retained retry state")
	}
	full.accept = false
	ev.ID = "cry-abandoned"
	f.EnqueueCry(ev)
	f.ReleaseCry(ev.ID)
	if len(f.pending) != 0 {
		t.Fatal("abandoned cry retained retry state")
	}
}
