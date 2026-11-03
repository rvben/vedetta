package camera

import (
	"image"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/cry"
	"github.com/rvben/vedetta/internal/snapshot"
)

func TestCrySnoozeKeepsEvidenceWithoutAlerts(t *testing.T) {
	events := make(chan Event, 2)
	cam := NewTestCamera("nursery")
	cam.events = events
	cam.config.Cry = config.CryCameraConfig{Enabled: true, Alerts: true}
	cam.cry = cry.NewMonitor(nil, "", .2, time.Minute, true, cam.emitCry)
	cam.SnoozeCry(time.Now().Add(time.Hour))
	cam.emitCry(cry.Transition{ID: "candidate", At: time.Now(), Score: .8})
	if ev := <-events; ev.Kind != EventKindCry || ev.Category != CategoryDetection || ev.TrackID != 0 {
		t.Fatal("snoozed recognition lost evidence or became an object/alert")
	}
	cam.SnoozeCry(time.Time{})
	cam.emitCry(cry.Transition{ID: "alert", At: time.Now(), Score: .8, Notify: true})
	if ev := <-events; ev.Category != CategoryAlert {
		t.Fatal("resumed alert missing")
	}
}

func TestCryAlertCarriesFreshPhotoAndRejectsStaleCachedFrame(t *testing.T) {
	cam := NewTestCamera("nursery")
	queue := make(chan Event, 2)
	cam.events = queue
	cam.eventSnapDir = t.TempDir()
	cam.cry = cry.NewMonitor(nil, "", .2, time.Minute, true, cam.emitCry)
	cam.SetTestFrame(image.NewRGBA(image.Rect(0, 0, 64, 48)))
	cam.SetTestLastFrameTime(time.Now())
	if !cam.emitCry(cry.Transition{ID: "cry-fresh", At: time.Now(), Notify: true}) {
		t.Fatal("fresh alert rejected")
	}
	ev := <-queue
	if ev.SnapshotImage == nil || ev.SnapshotPath == "" || !snapshot.ValidNotificationJPEG(ev.NotificationJPEG) {
		t.Fatal("fresh cry alert missing photo")
	}
	cam.SetTestLastFrameTime(time.Now().Add(-time.Minute))
	if !cam.emitCry(cry.Transition{ID: "cry-stale", At: time.Now(), Notify: true}) {
		t.Fatal("missing fresh video suppressed the audio alert")
	}
	ev = <-queue
	if ev.Category != CategoryAlert || ev.SnapshotImage != nil || ev.NotificationJPEG != nil {
		t.Fatal("stale photo attached to live alert")
	}
}
