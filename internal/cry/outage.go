package cry

import "time"

const (
	// OutageNotifyAfter is how long recognition must be out of service before
	// the people relying on its alerts are told. Shorter than this, a camera
	// reconnect or a restart would raise an alarm about nothing.
	OutageNotifyAfter = 5 * time.Minute

	// OutageRecoverAfter is how long recognition must work without a break
	// before an outage counts as over, so a camera that connects and drops
	// again does not produce a recovery and a second outage notice.
	OutageRecoverAfter = time.Minute
)

// Working reports whether recognition is scoring this camera's audio. Every
// other state means a cry would go unnoticed.
func (s Status) Working() bool {
	return s.State == "listening" || s.State == "crying"
}

// OutageNotice tells the people relying on crying alerts that recognition
// stopped working for a camera, or that it works again.
type OutageNotice struct {
	Camera string
	// Recovered is false for the start of an outage and true for its end.
	Recovered bool
	// Reason says why recognition is not working. Empty on recovery.
	Reason string
	// Since is when recognition stopped working.
	Since time.Time
	// At is when the notice was raised.
	At time.Time
}

type outageState struct {
	downSince time.Time
	upSince   time.Time
	reason    string
	// announced: at least one provider took the outage notice, so the
	// recovery is owed. notified: every provider took it, so it is no longer
	// proposed.
	announced bool
	notified  bool
	// recovering: at least one provider took the recovery notice. The people
	// it told believe recognition works, so a new failure is a new outage.
	recovering bool
}

// OutageTracker decides when a camera's recognition has been out of service
// long enough to tell someone, and when it is back. It holds no clock and no
// delivery: Observe proposes a notice and Delivered commits it, so a notice
// that could not be queued is proposed again on the next observation. A
// notice that only some providers took is recorded with PartlyDelivered: it
// stays proposed for the others, and what follows it is announced as if all
// had taken it.
//
// A tracker is not safe for concurrent use.
type OutageTracker struct {
	cameras map[string]*outageState
}

func NewOutageTracker() *OutageTracker {
	return &OutageTracker{cameras: make(map[string]*outageState)}
}

// Restore marks an outage that began at since as already announced, so a
// restart neither repeats the notice nor loses the recovery that follows it.
// everyone is false when a provider had yet to take the notice: it is then
// proposed again, for all providers, since which one missed it is not known.
func (t *OutageTracker) Restore(camera string, since time.Time, everyone bool) {
	t.cameras[camera] = &outageState{downSince: since, announced: true, notified: everyone}
}

// Observe records one reading of a camera's recognition status and returns
// the notice that is due, or nil. alerts says whether the camera's crying
// alerts would be delivered at now: an outage is not announced while they are
// switched off or snoozed, and is announced when the snooze ends if it still
// lasts. A recovery is announced whenever the outage was.
func (t *OutageTracker) Observe(now time.Time, camera string, status Status, alerts bool) *OutageNotice {
	s := t.cameras[camera]
	if s == nil {
		s = &outageState{}
		t.cameras[camera] = s
	}
	if !status.Working() {
		if s.recovering {
			*s = outageState{}
		}
		s.upSince = time.Time{}
		if s.downSince.IsZero() {
			s.downSince = now
		}
		s.reason = outageReason(status)
		if s.notified || !alerts || now.Sub(s.downSince) < OutageNotifyAfter {
			return nil
		}
		return &OutageNotice{Camera: camera, Reason: s.reason, Since: s.downSince, At: now}
	}
	if s.downSince.IsZero() {
		return nil
	}
	if s.upSince.IsZero() {
		s.upSince = now
	}
	if now.Sub(s.upSince) < OutageRecoverAfter {
		return nil
	}
	if s.announced {
		return &OutageNotice{Camera: camera, Recovered: true, Since: s.downSince, At: now}
	}
	delete(t.cameras, camera)
	return nil
}

// Delivered commits a notice returned by Observe once every notification
// provider took it. It reports whether this is the first the caller hears of
// the notice reaching anyone: false for a notice PartlyDelivered already
// recorded.
func (t *OutageTracker) Delivered(n OutageNotice) bool {
	s := t.cameras[n.Camera]
	if s == nil {
		return false
	}
	if n.Recovered {
		delete(t.cameras, n.Camera)
		return !s.recovering
	}
	first := !s.announced
	s.announced, s.notified = true, true
	return first
}

// PartlyDelivered records that some providers took a notice returned by
// Observe and others refused it. The notice is proposed again for those. The
// recovery of a partly delivered outage is announced even if they never take
// the outage, and a failure after a partly delivered recovery is a new outage.
// It reports whether this is the first provider to take the notice.
func (t *OutageTracker) PartlyDelivered(n OutageNotice) bool {
	s := t.cameras[n.Camera]
	if s == nil {
		return false
	}
	if n.Recovered {
		first := !s.recovering
		s.recovering = true
		return first
	}
	first := !s.announced
	s.announced = true
	return first
}

// Forget drops a camera whose recognition is no longer expected to work, such
// as one its operator stopped. It reports whether an outage had been announced
// for it, which the caller has persisted and must now clear.
func (t *OutageTracker) Forget(camera string) bool {
	s := t.cameras[camera]
	delete(t.cameras, camera)
	return s != nil && s.announced && !s.recovering
}

func outageReason(status Status) string {
	if status.Reason != "" {
		return status.Reason
	}
	switch status.State {
	case "starting":
		return "Recognition has not started"
	case "stopped":
		return "Recognition stopped"
	default:
		return "Recognition is not running"
	}
}
