package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/cry"
)

var cryOutageAt = time.Unix(1_800_000_000, 0)

func cryOutage(camera string) cry.OutageNotice {
	return cry.OutageNotice{
		Camera: camera, Reason: "Camera audio disconnected",
		Since: cryOutageAt.Add(-5 * time.Minute), At: cryOutageAt,
	}
}

func cryRecovery(camera string) cry.OutageNotice {
	return cry.OutageNotice{
		Camera: camera, Recovered: true,
		Since: cryOutageAt.Add(-135 * time.Minute), At: cryOutageAt,
	}
}

func decodePayload(t *testing.T, data []byte) pushPayload {
	t.Helper()
	var p pushPayload
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	return p
}

func TestCryStatusPayloadSaysWhatStoppedAndWhere(t *testing.T) {
	p := decodePayload(t, BuildCryStatusPayload(cryOutage("kids_bedroom_2")))
	if p.Title != "Crying alerts are not working" {
		t.Errorf("title = %q", p.Title)
	}
	if p.Body != "Kids Bedroom 2 · Camera audio disconnected" {
		t.Errorf("body = %q, want the camera and the reason", p.Body)
	}
	if p.URL != "/camera.html?name=kids_bedroom_2" {
		t.Errorf("url = %q, want the camera's live page", p.URL)
	}
	if p.TS != cryOutageAt.Unix() {
		t.Errorf("ts = %d, want %d", p.TS, cryOutageAt.Unix())
	}
	if p.Image != "" {
		t.Errorf("image = %q, want none", p.Image)
	}
}

func TestCryStatusRecoveryReplacesTheOutageCard(t *testing.T) {
	outage := decodePayload(t, BuildCryStatusPayload(cryOutage("nursery")))
	recovery := decodePayload(t, BuildCryStatusPayload(cryRecovery("nursery")))
	if recovery.Title != "Crying alerts are working again" {
		t.Errorf("title = %q", recovery.Title)
	}
	if recovery.Body != "Nursery · They were out for 2 hours 15 min" {
		t.Errorf("body = %q, want the camera and how long the outage lasted", recovery.Body)
	}
	if outage.Tag != recovery.Tag {
		t.Errorf("tags %q and %q differ: the recovery would stack on the outage card", outage.Tag, recovery.Tag)
	}
	// A real cry must never be replaced by a status card, nor replace one.
	if outage.Tag == "nursery:cry" {
		t.Errorf("status tag %q is the tag of a crying alert", outage.Tag)
	}
}

func TestRoughDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		20 * time.Second:  "less than a minute",
		time.Minute:       "1 minute",
		5 * time.Minute:   "5 minutes",
		time.Hour:         "1 hour",
		61 * time.Minute:  "1 hour 1 min",
		135 * time.Minute: "2 hours 15 min",
		48 * time.Hour:    "48 hours",
	} {
		if got := roughDuration(d); got != want {
			t.Errorf("roughDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestPushoverSendsCryStatusAsTextToCryDestinations(t *testing.T) {
	p := testPushover(t)
	p.cfg.Cameras = nil
	var warned strings.Builder
	p.logger = slog.New(slog.NewTextHandler(&warned, &slog.HandlerOptions{Level: slog.LevelWarn}))
	form := make(chan map[string]string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			return
		}
		defer r.MultipartForm.RemoveAll()
		got := map[string]string{"files": fmt.Sprint(len(r.MultipartForm.File))}
		for _, key := range []string{"title", "message", "url", "url_title", "timestamp"} {
			got[key] = r.FormValue(key)
		}
		form <- got
		fmt.Fprint(w, `{"status":1}`)
	}))
	defer srv.Close()
	p.endpoint = srv.URL

	if !p.EnqueueCryStatus(cryOutage("nursery")) || len(p.jobs) != 1 {
		t.Fatal("outage notice was not queued")
	}
	if err := p.send(context.Background(), <-p.jobs); err != nil {
		t.Fatal(err)
	}
	got := <-form
	want := map[string]string{
		"files": "0", "title": "Crying alerts are not working",
		"message": "Nursery · Camera audio disconnected",
		"url":     "https://vedetta.example.com/camera.html?name=nursery", "url_title": "Open Vedetta",
		"timestamp": fmt.Sprint(cryOutageAt.Unix()),
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %q, want %q", key, got[key], value)
		}
	}
	if warned.Len() != 0 {
		t.Errorf("a notice that has no photo by nature logged a warning: %s", warned.String())
	}
}

func TestPushoverCryStatusFollowsTheCryFilters(t *testing.T) {
	p := testPushover(t) // delivers for front_door only
	if !p.EnqueueCryStatus(cryOutage("nursery")) || len(p.jobs) != 0 {
		t.Fatal("notice sent for a camera this destination does not cover")
	}
	if !p.EnqueueCryStatus(cryOutage("front_door")) || len(p.jobs) != 1 {
		t.Fatal("notice not queued for a covered camera")
	}
	p.cfg.Events = []string{"doorbell", "activity"}
	if !p.EnqueueCryStatus(cryOutage("front_door")) || len(p.jobs) != 1 {
		t.Fatal("notice sent to a destination that does not take crying alerts")
	}
	p.cfg.Events = []string{"cry"}
	for len(p.jobs) < cap(p.jobs) {
		p.EnqueueCryStatus(cryOutage("front_door"))
	}
	if p.EnqueueCryStatus(cryOutage("front_door")) {
		t.Fatal("a full queue reported the notice as accepted, so it would never be retried")
	}
}

func TestWebPushCryStatusUsesTheCryPreferenceAndIgnoresCooldown(t *testing.T) {
	store := newFakeStore()
	seedAlice(store, "https://push.example/a")
	sender := &fakeSender{}
	d, capture := newLoggingDispatcher(t, store, sender)
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	defer func() { cancel(); d.Wait() }()

	// Outage and recovery within the cooldown window both arrive.
	if !d.EnqueueCryStatus(cryOutage("nursery")) || !d.EnqueueCryStatus(cryRecovery("nursery")) {
		t.Fatal("notice was not queued")
	}
	capture.waitForRecords(t, "push dispatch", 2)
	calls := sender.Calls()
	if len(calls) != 2 {
		t.Fatalf("sent %d pushes, want the outage and the recovery", len(calls))
	}
	if got := decodePayload(t, calls[0].Payload).Title; got != "Crying alerts are not working" {
		t.Errorf("first push title = %q", got)
	}
	if got := decodePayload(t, calls[1].Payload).Title; got != "Crying alerts are working again" {
		t.Errorf("second push title = %q", got)
	}

	// Someone who switched crying alerts off for this camera is not told
	// about their outage either.
	store.mu.Lock()
	store.disabledPrefs["alice|nursery|baby_cry"] = true
	store.mu.Unlock()
	d.EnqueueCryStatus(cryOutage("nursery"))
	capture.waitForRecords(t, "push dispatch", 3)
	if got := len(sender.Calls()); got != 2 {
		t.Errorf("sent %d pushes, want still 2 once crying alerts are off for the camera", got)
	}
}

type statusProvider struct {
	testProvider
	notices int
}

func (p *statusProvider) EnqueueCryStatus(cry.OutageNotice) bool { p.notices++; return p.accept }

func TestFanoutCryStatusRetriesOnlyTheProviderThatRefused(t *testing.T) {
	full := &statusProvider{}
	ready := &statusProvider{testProvider: testProvider{accept: true}}
	plain := &testProvider{}
	f := NewFanout(full, ready, plain)
	n := cryOutage("nursery")
	if f.EnqueueCryStatus(n) {
		t.Fatal("notice reported as accepted while one provider's queue was full")
	}
	full.accept = true
	if !f.EnqueueCryStatus(n) {
		t.Fatal("notice not accepted on the retry")
	}
	if full.notices != 2 || ready.notices != 1 {
		t.Fatalf("providers were asked %d and %d times, want 2 and 1: the retry must not send twice", full.notices, ready.notices)
	}
	if len(f.pending) != 0 || len(f.cryStatus) != 0 {
		t.Fatal("completed fanout leaked pending state")
	}
	// The recovery is a different notice, not a retry of the outage.
	if !f.EnqueueCryStatus(cryRecovery("nursery")) || ready.notices != 2 {
		t.Fatal("recovery was not handed to a provider that took the outage")
	}
}

// An outage one provider took has reached people, although the fanout still
// retries it. A provider that carries no such notices reaches nobody.
func TestFanoutCryStatusReachedNeedsAProviderThatDeliversIt(t *testing.T) {
	full := &statusProvider{}
	ready := &statusProvider{testProvider: testProvider{accept: true}}
	n := cryOutage("nursery")

	deaf := NewFanout(full, &testProvider{})
	if deaf.EnqueueCryStatus(n) || deaf.CryStatusReached(n) {
		t.Fatal("an outage only a full queue could deliver was reported as having reached someone")
	}
	f := NewFanout(full, ready)
	if f.CryStatusReached(n) {
		t.Fatal("a notice that was never enqueued was reported as having reached someone")
	}
	if f.EnqueueCryStatus(n) {
		t.Fatal("notice reported as accepted while one provider's queue was full")
	}
	if !f.CryStatusReached(n) {
		t.Fatal("an outage one provider took was not reported as having reached someone")
	}
}

// The recovery ends the retries of its outage. A provider that never took the
// outage gets the recovery alone, and nothing of the outage is kept.
func TestFanoutCryRecoveryEndsTheRetriesOfItsOutage(t *testing.T) {
	full := &statusProvider{}
	ready := &statusProvider{testProvider: testProvider{accept: true}}
	f := NewFanout(full, ready)
	outage := cryOutage("nursery")
	if f.EnqueueCryStatus(outage) {
		t.Fatal("notice reported as accepted while one provider's queue was full")
	}
	full.accept = true
	recovery := outage
	recovery.Recovered, recovery.Reason = true, ""
	if !f.EnqueueCryStatus(recovery) {
		t.Fatal("recovery was not accepted")
	}
	if full.notices != 2 || ready.notices != 2 {
		t.Fatalf("providers were asked %d and %d times, want 2 and 2", full.notices, ready.notices)
	}
	if len(f.cryStatus) != 0 {
		t.Fatalf("%d deliveries kept after the recovery, want nothing kept of the outage", len(f.cryStatus))
	}
}

// A provider whose settings leave the camera out accepts the notice and sends
// it to nobody, so it does not make the notice one that reached someone.
func TestFanoutCryStatusReachedIgnoresAProviderThatFiltersTheCamera(t *testing.T) {
	filtered := testPushover(t)
	filtered.cfg.Cameras = []string{"hallway"}
	if filtered.DeliversCryStatus("nursery") || !filtered.DeliversCryStatus("hallway") {
		t.Fatal("the provider's camera filter does not apply to crying status notices")
	}

	f := NewFanout(&statusProvider{}, filtered)
	n := cryOutage("nursery")
	if f.EnqueueCryStatus(n) {
		t.Fatal("notice reported as accepted while one provider's queue was full")
	}
	if f.CryStatusReached(n) {
		t.Fatal("an outage that only a provider filtering the camera accepted was reported as having reached someone")
	}
}
