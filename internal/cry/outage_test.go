package cry

import (
	"testing"
	"time"
)

var (
	outageStart = time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	down        = Status{State: "unavailable", Reason: "Camera audio disconnected", Alerts: true}
	up          = Status{State: "listening", Alerts: true}
)

// observeEvery feeds the tracker one reading every 15 seconds over a span and
// returns the notices it proposed, committing each as delivered.
func observeEvery(t *testing.T, tr *OutageTracker, from time.Time, span time.Duration, status Status, alerts bool) []OutageNotice {
	t.Helper()
	var notices []OutageNotice
	for at := from; !at.After(from.Add(span)); at = at.Add(15 * time.Second) {
		if n := tr.Observe(at, "nursery", status, alerts); n != nil {
			notices = append(notices, *n)
			tr.Delivered(*n)
		}
	}
	return notices
}

func TestOutageIsAnnouncedAfterFiveMinutesAndOnce(t *testing.T) {
	tr := NewOutageTracker()
	if got := observeEvery(t, tr, outageStart, OutageNotifyAfter-15*time.Second, down, true); len(got) != 0 {
		t.Fatalf("announced %d notices before the outage lasted five minutes: %+v", len(got), got)
	}
	n := tr.Observe(outageStart.Add(OutageNotifyAfter), "nursery", down, true)
	if n == nil {
		t.Fatal("no notice once the outage lasted five minutes")
	}
	if n.Recovered || n.Reason != "Camera audio disconnected" || !n.Since.Equal(outageStart) {
		t.Errorf("notice = %+v, want an outage since %v with the monitor's reason", *n, outageStart)
	}
	tr.Delivered(*n)
	if got := observeEvery(t, tr, outageStart.Add(OutageNotifyAfter), time.Hour, down, true); len(got) != 0 {
		t.Errorf("the same outage was announced %d more times", len(got))
	}
}

func TestOutageNoticeIsProposedAgainUntilItIsDelivered(t *testing.T) {
	tr := NewOutageTracker()
	tr.Observe(outageStart, "nursery", down, true)
	at := outageStart.Add(OutageNotifyAfter)
	if tr.Observe(at, "nursery", down, true) == nil {
		t.Fatal("no notice once the outage lasted five minutes")
	}
	// The providers' queues were full: nothing was committed.
	again := tr.Observe(at.Add(15*time.Second), "nursery", down, true)
	if again == nil {
		t.Fatal("an undelivered notice was not proposed again")
	}
	tr.Delivered(*again)
	if tr.Observe(at.Add(30*time.Second), "nursery", down, true) != nil {
		t.Error("a delivered notice was proposed again")
	}
}

func TestOutageIsNotAnnouncedWhileAlertsAreSnoozed(t *testing.T) {
	tr := NewOutageTracker()
	if got := observeEvery(t, tr, outageStart, time.Hour, down, false); len(got) != 0 {
		t.Fatalf("announced an outage while alerts were snoozed: %+v", got)
	}
	n := tr.Observe(outageStart.Add(time.Hour+15*time.Second), "nursery", down, true)
	if n == nil {
		t.Fatal("an outage that outlasted the snooze was not announced when the snooze ended")
	}
	if !n.Since.Equal(outageStart) {
		t.Errorf("outage since %v, want the moment recognition stopped, %v", n.Since, outageStart)
	}
}

func TestRecoveryIsAnnouncedAfterAMinuteOfWorking(t *testing.T) {
	tr := NewOutageTracker()
	observeEvery(t, tr, outageStart, OutageNotifyAfter, down, true)
	back := outageStart.Add(10 * time.Minute)
	if got := observeEvery(t, tr, back, OutageRecoverAfter-15*time.Second, up, true); len(got) != 0 {
		t.Fatalf("recovery announced before recognition worked for a minute: %+v", got)
	}
	n := tr.Observe(back.Add(OutageRecoverAfter), "nursery", up, true)
	if n == nil || !n.Recovered {
		t.Fatalf("notice = %+v, want a recovery", n)
	}
	if !n.Since.Equal(outageStart) {
		t.Errorf("recovery names an outage since %v, want %v", n.Since, outageStart)
	}
	tr.Delivered(*n)
	if got := observeEvery(t, tr, back.Add(OutageRecoverAfter), time.Hour, up, true); len(got) != 0 {
		t.Errorf("recovery announced %d more times", len(got))
	}
}

func TestRecoveryIsAnnouncedEvenWhileAlertsAreSnoozed(t *testing.T) {
	tr := NewOutageTracker()
	observeEvery(t, tr, outageStart, OutageNotifyAfter, down, true)
	back := outageStart.Add(10 * time.Minute)
	got := observeEvery(t, tr, back, OutageRecoverAfter, up, false)
	if len(got) != 1 || !got[0].Recovered {
		t.Fatalf("notices = %+v, want one recovery: whoever was told about the outage is told it ended", got)
	}
}

func TestACameraThatKeepsDroppingIsNeitherRecoveredNorAnnouncedTwice(t *testing.T) {
	tr := NewOutageTracker()
	observeEvery(t, tr, outageStart, OutageNotifyAfter, down, true)
	at := outageStart.Add(OutageNotifyAfter)
	for range 20 {
		at = at.Add(15 * time.Second)
		if n := tr.Observe(at, "nursery", up, true); n != nil {
			t.Fatalf("notice %+v for a camera that worked for seconds", *n)
		}
		at = at.Add(30 * time.Second)
		if n := tr.Observe(at, "nursery", down, true); n != nil {
			t.Fatalf("notice %+v for an outage that was already announced", *n)
		}
	}
}

func TestAShortOutageIsNeverAnnounced(t *testing.T) {
	tr := NewOutageTracker()
	if got := observeEvery(t, tr, outageStart, 2*time.Minute, down, true); len(got) != 0 {
		t.Fatalf("announced a two minute outage: %+v", got)
	}
	back := outageStart.Add(2*time.Minute + 15*time.Second)
	if got := observeEvery(t, tr, back, time.Hour, up, true); len(got) != 0 {
		t.Fatalf("announced a recovery from an outage nobody was told about: %+v", got)
	}
	// The earlier break does not count towards the next outage.
	later := back.Add(2 * time.Hour)
	if got := observeEvery(t, tr, later, OutageNotifyAfter-15*time.Second, down, true); len(got) != 0 {
		t.Errorf("a new outage was announced early: %+v", got)
	}
}

func TestRestoredOutageIsNotAnnouncedAgainAndItsRecoveryIs(t *testing.T) {
	tr := NewOutageTracker()
	tr.Restore("nursery", outageStart, true)
	restart := outageStart.Add(3 * time.Hour)
	if got := observeEvery(t, tr, restart, time.Hour, down, true); len(got) != 0 {
		t.Fatalf("an outage announced before the restart was announced again: %+v", got)
	}
	got := observeEvery(t, tr, restart.Add(2*time.Hour), OutageRecoverAfter, up, true)
	if len(got) != 1 || !got[0].Recovered || !got[0].Since.Equal(outageStart) {
		t.Fatalf("notices = %+v, want one recovery of the outage since %v", got, outageStart)
	}
}

func TestForgetReportsWhetherAnOutageWasAnnounced(t *testing.T) {
	tr := NewOutageTracker()
	tr.Observe(outageStart, "nursery", down, true)
	if tr.Forget("nursery") {
		t.Error("Forget reported an announced outage for one that was not")
	}
	observeEvery(t, tr, outageStart, OutageNotifyAfter, down, true)
	if !tr.Forget("nursery") {
		t.Error("Forget did not report the announced outage")
	}
	if n := tr.Observe(outageStart.Add(time.Hour), "nursery", up, true); n != nil {
		t.Errorf("notice %+v for a forgotten camera", *n)
	}
}

func TestStatesThatAreNotScoringAudioAreOutages(t *testing.T) {
	for state, working := range map[string]bool{
		"listening": true, "crying": true,
		"starting": false, "unavailable": false, "unsupported": false, "overloaded": false, "stopped": false,
	} {
		if got := (Status{State: state}).Working(); got != working {
			t.Errorf("Working() for %q = %v, want %v", state, got, working)
		}
	}
	tr := NewOutageTracker()
	tr.Observe(outageStart, "nursery", Status{State: "starting"}, true)
	n := tr.Observe(outageStart.Add(OutageNotifyAfter), "nursery", Status{State: "starting"}, true)
	if n == nil || n.Reason == "" {
		t.Fatalf("notice = %+v, want an outage with a reason for a monitor that never started", n)
	}
}

// An outage that one provider took and another refused stays proposed for the
// other, and its recovery is announced: someone was told recognition stopped.
func TestPartlyDeliveredOutageIsProposedAgainAndRecovered(t *testing.T) {
	tr := NewOutageTracker()
	at := outageStart.Add(OutageNotifyAfter)
	tr.Observe(outageStart, "nursery", down, true)
	n := tr.Observe(at, "nursery", down, true)
	if n == nil {
		t.Fatal("no outage proposed after the notify delay")
	}
	if !tr.PartlyDelivered(*n) {
		t.Error("the first provider to take the outage was not reported as the first")
	}
	if tr.PartlyDelivered(*n) {
		t.Error("a second partial delivery was reported as the first")
	}
	if again := tr.Observe(at.Add(15*time.Second), "nursery", down, true); again == nil {
		t.Fatal("the outage is no longer proposed although one provider has not taken it")
	}
	if tr.Delivered(*n) {
		t.Error("completing a partly delivered outage was reported as its first delivery")
	}
	if again := tr.Observe(at.Add(30*time.Second), "nursery", down, true); again != nil {
		t.Fatalf("an outage every provider took was proposed again: %+v", *again)
	}

	partly := NewOutageTracker()
	partly.Observe(outageStart, "nursery", down, true)
	partly.PartlyDelivered(*partly.Observe(at, "nursery", down, true))
	got := observeEvery(t, partly, at.Add(time.Minute), OutageRecoverAfter, up, true)
	if len(got) != 1 || !got[0].Recovered || !got[0].Since.Equal(outageStart) {
		t.Fatalf("notices = %+v, want the recovery of an outage only one provider took", got)
	}
}

func TestDeliveredReportsTheFirstDelivery(t *testing.T) {
	tr := NewOutageTracker()
	tr.Observe(outageStart, "nursery", down, true)
	n := tr.Observe(outageStart.Add(OutageNotifyAfter), "nursery", down, true)
	if !tr.Delivered(*n) {
		t.Error("the delivery of an outage was not reported as its first")
	}
	if tr.Delivered(OutageNotice{Camera: "hallway"}) {
		t.Error("an outage of a camera the tracker does not follow was reported as delivered")
	}
}

// A recovery one provider took has told people recognition works. When it
// fails again before the other provider takes the recovery, that is a new
// outage, announced like any other.
func TestFailureAfterAPartlyDeliveredRecoveryIsANewOutage(t *testing.T) {
	tr := NewOutageTracker()
	observeEvery(t, tr, outageStart, OutageNotifyAfter, down, true)
	back := outageStart.Add(time.Hour)
	tr.Observe(back, "nursery", up, true)
	recovery := tr.Observe(back.Add(OutageRecoverAfter), "nursery", up, true)
	if recovery == nil || !recovery.Recovered {
		t.Fatalf("notice = %+v, want the recovery", recovery)
	}
	if !tr.PartlyDelivered(*recovery) {
		t.Error("the first provider to take the recovery was not reported as the first")
	}
	if again := tr.Observe(back.Add(2*time.Minute), "nursery", up, true); again == nil || !again.Recovered {
		t.Fatalf("notice = %+v, want the recovery proposed again for the provider that has not taken it", again)
	}
	if tr.Forget("nursery") {
		t.Error("Forget reported an announced outage for one whose recovery was announced")
	}

	tr = NewOutageTracker()
	observeEvery(t, tr, outageStart, OutageNotifyAfter, down, true)
	tr.Observe(back, "nursery", up, true)
	tr.PartlyDelivered(*tr.Observe(back.Add(OutageRecoverAfter), "nursery", up, true))
	failedAgain := back.Add(10 * time.Minute)
	got := observeEvery(t, tr, failedAgain, OutageNotifyAfter, down, true)
	if len(got) != 1 || got[0].Recovered || !got[0].Since.Equal(failedAgain) {
		t.Fatalf("notices = %+v, want a new outage since %v", got, failedAgain)
	}
}

// After a restart nothing records which provider had not taken the outage, so
// it is offered again, and the recovery still follows it.
func TestRestoredPartlyDeliveredOutageIsProposedAgain(t *testing.T) {
	tr := NewOutageTracker()
	tr.Restore("nursery", outageStart, false)
	restart := outageStart.Add(time.Hour)
	n := tr.Observe(restart, "nursery", down, true)
	if n == nil || n.Recovered || !n.Since.Equal(outageStart) {
		t.Fatalf("notice = %+v, want the outage since %v offered again", n, outageStart)
	}
	if tr.Delivered(*n) {
		t.Error("an outage announced before the restart was reported as newly announced")
	}
	got := observeEvery(t, tr, restart.Add(time.Hour), OutageRecoverAfter, up, true)
	if len(got) != 1 || !got[0].Recovered {
		t.Fatalf("notices = %+v, want the recovery", got)
	}
}
