package main

import (
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/cry"
	"github.com/rvben/vedetta/internal/notify"
	"github.com/rvben/vedetta/internal/storage"
)

type memorySettings struct {
	values map[string]string
	fail   error
}

func (m *memorySettings) GetSetting(key string) (string, error) { return m.values[key], m.fail }
func (m *memorySettings) SetSetting(key, value string) error {
	if m.fail != nil {
		return m.fail
	}
	m.values[key] = value
	return nil
}

type recordingSender struct {
	refuse  bool
	notices []cry.OutageNotice
}

func (s *recordingSender) EnqueueCryStatus(n cry.OutageNotice) bool {
	if s.refuse {
		return false
	}
	s.notices = append(s.notices, n)
	return true
}

// A fullQueue is a provider that carries crying status notices and whose
// queue has no room for one until the test makes some.
type fullQueue struct {
	room    bool
	asked   int
	notices []cry.OutageNotice
}

func (q *fullQueue) EnqueueCryStatus(n cry.OutageNotice) bool {
	q.asked++
	if q.room {
		q.notices = append(q.notices, n)
	}
	return q.room
}
func (*fullQueue) Enqueue(camera.Event)                  {}
func (*fullQueue) EnqueueDoorbell(camera.Event) bool     { return true }
func (*fullQueue) EnqueueActivity(storage.Activity) bool { return true }

func (*recordingSender) Enqueue(camera.Event)                  {}
func (*recordingSender) EnqueueDoorbell(camera.Event) bool     { return true }
func (*recordingSender) EnqueueActivity(storage.Activity) bool { return true }

// cryRig drives a watcher over one camera whose status the test sets.
type cryRig struct {
	watcher  *cryOutageWatcher
	sender   *recordingSender
	settings *memorySettings
	status   camera.CameraStatus
	now      time.Time
}

func newCryRig(settings *memorySettings) *cryRig {
	if settings == nil {
		settings = &memorySettings{values: map[string]string{}}
	}
	r := &cryRig{
		sender: &recordingSender{}, settings: settings,
		now: time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC),
	}
	r.set("unavailable", "Camera audio disconnected")
	r.watcher = newCryOutageWatcher(func() []camera.CameraStatus { return []camera.CameraStatus{r.status} }, r.sender, settings)
	r.watcher.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return r
}

func (r *cryRig) set(state, reason string) {
	r.status = camera.CameraStatus{Name: "nursery", Cry: &cry.Status{State: state, Reason: reason, Alerts: true}}
}

// pass runs the watcher at its real cadence for a span of simulated time.
func (r *cryRig) pass(span time.Duration) {
	for end := r.now.Add(span); !r.now.After(end); r.now = r.now.Add(cryOutagePollInterval) {
		r.watcher.tick(r.now)
	}
}

const nurseryOutageKey = "cry_outage:nursery"

func TestCryOutageIsSentOnceAndItsRecoveryOnce(t *testing.T) {
	r := newCryRig(nil)
	started := r.now
	r.pass(4 * time.Minute)
	if len(r.sender.notices) != 0 {
		t.Fatalf("notice after four minutes: %+v", r.sender.notices)
	}
	r.pass(2 * time.Hour)
	if len(r.sender.notices) != 1 || r.sender.notices[0].Recovered {
		t.Fatalf("notices = %+v, want exactly one outage", r.sender.notices)
	}
	if got := r.sender.notices[0]; got.Camera != "nursery" || got.Reason != "Camera audio disconnected" || !got.Since.Equal(started) {
		t.Errorf("outage notice = %+v", got)
	}
	if r.settings.values[nurseryOutageKey] != started.Format(time.RFC3339) {
		t.Errorf("stored outage = %q, want its start %s", r.settings.values[nurseryOutageKey], started.Format(time.RFC3339))
	}

	r.set("listening", "")
	r.pass(time.Hour)
	if len(r.sender.notices) != 2 || !r.sender.notices[1].Recovered {
		t.Fatalf("notices = %+v, want the outage and one recovery", r.sender.notices)
	}
	if r.settings.values[nurseryOutageKey] != "" {
		t.Errorf("stored outage = %q after recovery, want it cleared", r.settings.values[nurseryOutageKey])
	}
}

// nursery is the rig's camera as configured to send crying alerts.
var nursery = []config.CameraConfig{{Name: "nursery", Cry: config.CryCameraConfig{Enabled: true, Alerts: true}}}

func TestCryOutageSurvivesARestart(t *testing.T) {
	first := newCryRig(nil)
	started := first.now
	first.pass(10 * time.Minute)
	if len(first.sender.notices) != 1 {
		t.Fatalf("notices before the restart = %+v, want one outage", first.sender.notices)
	}

	// A new process over the same settings, the camera still gone.
	second := newCryRig(first.settings)
	second.now = started.Add(time.Hour)
	second.watcher.restore(nursery)
	second.pass(time.Hour)
	if len(second.sender.notices) != 0 {
		t.Fatalf("the outage was announced again after a restart: %+v", second.sender.notices)
	}
	second.set("listening", "")
	second.pass(5 * time.Minute)
	if len(second.sender.notices) != 1 || !second.sender.notices[0].Recovered || !second.sender.notices[0].Since.Equal(started) {
		t.Fatalf("notices = %+v, want the recovery of the outage that began at %v", second.sender.notices, started)
	}
}

func TestCryOutageIsRetriedWhenTheQueueIsFull(t *testing.T) {
	r := newCryRig(nil)
	r.sender.refuse = true
	r.pass(10 * time.Minute)
	if r.settings.values[nurseryOutageKey] != "" {
		t.Fatal("an outage nobody was told about was stored as announced")
	}
	r.sender.refuse = false
	r.pass(time.Minute)
	if len(r.sender.notices) != 1 {
		t.Fatalf("notices = %+v, want the outage once the queue had room", r.sender.notices)
	}
}

func TestCryOutageWaitsForASnoozeToEnd(t *testing.T) {
	r := newCryRig(nil)
	snoozeEnd := r.now.Add(time.Hour)
	r.status.Cry.SnoozedUntil = snoozeEnd
	r.pass(59 * time.Minute)
	if len(r.sender.notices) != 0 {
		t.Fatalf("notice during a snooze: %+v", r.sender.notices)
	}
	r.pass(2 * time.Minute)
	if len(r.sender.notices) != 1 {
		t.Fatalf("notices = %+v, want the outage once the snooze ended", r.sender.notices)
	}
}

func TestCryOutageIgnoresCamerasWithoutAlertsToMiss(t *testing.T) {
	for name, change := range map[string]func(*camera.CameraStatus){
		"stopped by its operator": func(st *camera.CameraStatus) { st.Stopped = true },
		"observation only":        func(st *camera.CameraStatus) { st.Cry.Alerts = false },
		"no recognition":          func(st *camera.CameraStatus) { st.Cry = nil },
	} {
		t.Run(name, func(t *testing.T) {
			r := newCryRig(nil)
			change(&r.status)
			r.pass(time.Hour)
			if len(r.sender.notices) != 0 {
				t.Fatalf("notices = %+v, want none", r.sender.notices)
			}
		})
	}
}

func TestStoppingACameraClearsItsAnnouncedOutage(t *testing.T) {
	r := newCryRig(nil)
	r.pass(10 * time.Minute)
	if r.settings.values[nurseryOutageKey] == "" {
		t.Fatal("outage was not stored")
	}
	r.status.Stopped = true
	r.pass(time.Minute)
	if r.settings.values[nurseryOutageKey] != "" {
		t.Fatal("a stopped camera kept its stored outage, so a later start would announce a recovery")
	}
	r.status.Stopped = false
	r.set("listening", "")
	r.pass(time.Hour)
	if len(r.sender.notices) != 1 {
		t.Fatalf("notices = %+v, want only the original outage", r.sender.notices)
	}
}

func TestCryOutageIsLoggedWithoutANotificationProvider(t *testing.T) {
	r := newCryRig(nil)
	r.watcher.sender = nil
	r.pass(10 * time.Minute)
	if r.settings.values[nurseryOutageKey] == "" {
		t.Fatal("without a provider the outage was never settled, so it would be reported at every tick")
	}
}

func TestCryOutageRestoreToleratesBadState(t *testing.T) {
	settings := &memorySettings{values: map[string]string{nurseryOutageKey: "yesterday"}}
	r := newCryRig(settings)
	r.watcher.restore(nursery)
	if settings.values[nurseryOutageKey] != "" {
		t.Error("an unreadable stored outage was kept")
	}
	r.pass(10 * time.Minute)
	if len(r.sender.notices) != 1 {
		t.Fatalf("notices = %+v, want the outage announced, since nothing proves it was", r.sender.notices)
	}

	broken := newCryRig(&memorySettings{values: map[string]string{}, fail: errors.New("database is locked")})
	broken.watcher.restore(nursery)
	broken.pass(10 * time.Minute)
	if len(broken.sender.notices) != 1 {
		t.Fatalf("notices = %+v, want the outage sent even though it cannot be stored", broken.sender.notices)
	}
}

// With two providers, one can take the outage while the other's queue is
// full. The people the first told are owed the recovery, also when the second
// never takes the outage.
func TestCryOutageThatOnlyOneProviderTookIsStillRecovered(t *testing.T) {
	r := newCryRig(nil)
	started := r.now
	full := &fullQueue{}
	r.watcher.sender = notify.NewFanout(r.sender, full)

	r.pass(10 * time.Minute)
	if len(r.sender.notices) != 1 || r.sender.notices[0].Recovered {
		t.Fatalf("notices = %+v, want the outage sent once to the provider that has room", r.sender.notices)
	}
	if full.asked < 2 {
		t.Fatalf("the provider with a full queue was asked %d times, want a retry at every tick", full.asked)
	}
	if got, want := r.settings.values[nurseryOutageKey], started.Format(time.RFC3339)+cryOutagePartlySuffix; got != want {
		t.Fatalf("stored outage = %q, want %q: a restart would lose the recovery", got, want)
	}

	full.room = true
	r.set("listening", "")
	r.pass(5 * time.Minute)
	if len(r.sender.notices) != 2 || !r.sender.notices[1].Recovered {
		t.Fatalf("notices = %+v, want the recovery for the provider that announced the outage", r.sender.notices)
	}
	if len(full.notices) != 1 || !full.notices[0].Recovered {
		t.Fatalf("the other provider got %+v, want the recovery alone: its outage notice is out of date", full.notices)
	}
	if r.settings.values[nurseryOutageKey] != "" {
		t.Errorf("stored outage = %q after recovery, want it cleared", r.settings.values[nurseryOutageKey])
	}

	// The next outage is a new notice for both providers.
	r.set("unavailable", "Camera audio disconnected")
	r.pass(10 * time.Minute)
	if len(r.sender.notices) != 3 || r.sender.notices[2].Recovered {
		t.Fatalf("notices = %+v, want the next outage announced", r.sender.notices)
	}
	if len(full.notices) != 2 || full.notices[1].Recovered {
		t.Fatalf("the other provider got %+v, want the next outage too", full.notices)
	}
}

// An outage stored for a camera that no longer sends crying alerts has no
// recovery coming. Left in place, it passes for the announcement of the next
// outage once alerts are switched back on.
func TestCryOutageStoredForACameraWithoutAlertsIsCleared(t *testing.T) {
	alerting := func(on bool) []config.CameraConfig {
		return []config.CameraConfig{{Name: "nursery", Cry: config.CryCameraConfig{Enabled: on, Alerts: on}}}
	}
	first := newCryRig(nil)
	first.pass(10 * time.Minute)
	if first.settings.values[nurseryOutageKey] == "" {
		t.Fatal("the outage was not stored")
	}

	off := newCryRig(first.settings)
	if off.watcher.restore(alerting(false)) {
		t.Error("restore reported a camera to watch although none sends crying alerts")
	}
	if got := first.settings.values[nurseryOutageKey]; got != "" {
		t.Fatalf("stored outage = %q with crying alerts switched off, want it cleared", got)
	}

	on := newCryRig(first.settings)
	on.now = first.now.Add(24 * time.Hour)
	if !on.watcher.restore(alerting(true)) {
		t.Fatal("restore reported no camera to watch")
	}
	on.pass(10 * time.Minute)
	if len(on.sender.notices) != 1 || on.sender.notices[0].Recovered {
		t.Fatalf("notices = %+v, want the new outage announced", on.sender.notices)
	}
}

// A snooze outlives a restart also when the model fails to load: recognition
// is then out of service, and the snooze is what keeps that notice quiet.
func TestCrySnoozeIsRestoredWhenTheModelDoesNotLoad(t *testing.T) {
	db, err := storage.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	until := time.Now().Add(6 * time.Hour).Truncate(time.Second)
	if err := db.SetSetting("cry_snooze:nursery", until.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Cameras: []config.CameraConfig{{Name: "nursery", Cry: config.CryCameraConfig{Enabled: true, Alerts: true}}},
		Cry:     config.CryConfig{ModelPath: filepath.Join(t.TempDir(), "absent.onnx")},
	}
	sub := &subsystems{manager: camera.NewManager(cfg.Cameras, nil, config.MotionConfig{}, nil, nil, nil, nil, t.TempDir(), 80, t.TempDir(), nil, nil, t.TempDir(), nil, nil)}

	initCryRecognition(cfg, db, sub)

	statuses := sub.manager.CameraStatuses()
	if len(statuses) != 1 || statuses[0].Cry == nil {
		t.Fatalf("statuses = %+v, want the nursery with a recognition status", statuses)
	}
	if statuses[0].Cry.Working() {
		t.Fatal("recognition reports working without a model; the test did not reach the failure path")
	}
	if got := statuses[0].Cry.SnoozedUntil; !got.Equal(until) {
		t.Fatalf("snoozed until %v, want the stored %v", got, until)
	}
}

// A restart forgets which provider had not taken the outage. It is offered
// again to all of them, and stored as complete once they have it.
func TestCryOutageOneProviderMissedIsOfferedAgainAfterARestart(t *testing.T) {
	first := newCryRig(nil)
	started := first.now
	first.watcher.sender = notify.NewFanout(first.sender, &fullQueue{})
	first.pass(10 * time.Minute)
	if len(first.sender.notices) != 1 {
		t.Fatalf("notices before the restart = %+v, want the outage once", first.sender.notices)
	}

	second := newCryRig(first.settings)
	second.now = started.Add(time.Hour)
	missed := &fullQueue{room: true}
	second.watcher.sender = notify.NewFanout(second.sender, missed)
	second.watcher.restore(nursery)
	second.pass(10 * time.Minute)
	if len(missed.notices) != 1 || missed.notices[0].Recovered || !missed.notices[0].Since.Equal(started) {
		t.Fatalf("the provider that missed the outage got %+v after the restart, want the outage since %v", missed.notices, started)
	}
	if len(second.sender.notices) != 1 {
		t.Fatalf("the other provider got %+v, want the outage once more and no more", second.sender.notices)
	}
	if got := second.settings.values[nurseryOutageKey]; got != started.Format(time.RFC3339) {
		t.Fatalf("stored outage = %q, want %s now that every provider has it", got, started.Format(time.RFC3339))
	}
}

// One provider told its people recognition works again while the other's
// queue was full. A failure that follows is theirs to hear about, whether or
// not the other provider ever takes the recovery.
func TestCryFailureAfterARecoveryOnlyOneProviderTookIsAnnounced(t *testing.T) {
	r := newCryRig(nil)
	slow := &fullQueue{room: true}
	r.watcher.sender = notify.NewFanout(r.sender, slow)
	r.pass(10 * time.Minute)
	if len(r.sender.notices) != 1 || len(slow.notices) != 1 {
		t.Fatalf("notices = %+v and %+v, want the outage at both providers", r.sender.notices, slow.notices)
	}

	slow.room = false
	r.set("listening", "")
	r.pass(5 * time.Minute)
	if len(r.sender.notices) != 2 || !r.sender.notices[1].Recovered {
		t.Fatalf("notices = %+v, want the recovery sent once to the provider that has room", r.sender.notices)
	}
	if r.settings.values[nurseryOutageKey] != "" {
		t.Fatalf("stored outage = %q after people were told it is over", r.settings.values[nurseryOutageKey])
	}

	failedAgain := r.now
	r.set("unavailable", "Camera audio disconnected")
	slow.room = true
	r.pass(10 * time.Minute)
	if len(r.sender.notices) != 3 || r.sender.notices[2].Recovered || !r.sender.notices[2].Since.Equal(failedAgain) {
		t.Fatalf("notices = %+v, want a new outage since %v", r.sender.notices, failedAgain)
	}
	if len(slow.notices) != 2 || slow.notices[1].Recovered {
		t.Fatalf("the other provider got %+v, want the new outage and not the recovery it is too late for", slow.notices)
	}
}
