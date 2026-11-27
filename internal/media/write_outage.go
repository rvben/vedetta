package media

import "time"

// writeOutageReportInterval is how often a write outage that keeps pausing
// recording is reported again.
const writeOutageReportInterval = 5 * time.Minute

// writeOutage tracks a run of write failures so that it is reported when it
// starts, at intervals while it lasts and when it ends. Recording retries a
// failing volume every diskPauseRetryInterval for as long as it is out, and
// each retry fails the same way.
type writeOutage struct {
	since      time.Time // first failure of the run; zero while writes succeed
	paused     bool      // the run has paused recording at least once
	lastReport time.Time // last pause that was reported
	unreported int       // pauses since lastReport
}

// failure records a write failure that does not pause recording, and reports
// whether to log it. The failures that lead to the first pause are logged;
// those of a retry during the same outage repeat what is already known.
func (o *writeOutage) failure(now time.Time) bool {
	if o.since.IsZero() {
		o.since = now
	}
	return !o.paused
}

// pause records that the run paused recording, and reports whether to log it
// and how many pauses went unlogged since the last one that was.
func (o *writeOutage) pause(now time.Time) (report bool, unreported int) {
	if o.since.IsZero() {
		o.since = now
	}
	if o.paused && now.Sub(o.lastReport) < writeOutageReportInterval {
		o.unreported++
		return false, 0
	}
	unreported = o.unreported
	o.paused = true
	o.lastReport = now
	o.unreported = 0
	return true, unreported
}

// ongoing reports whether a write has failed and none has succeeded since.
func (o *writeOutage) ongoing() bool { return !o.since.IsZero() }

// recovered records a successful write. It reports how long the outage
// lasted, and whether it ever paused recording and so is worth a line.
func (o *writeOutage) recovered(now time.Time) (lasted time.Duration, wasPaused bool) {
	if o.since.IsZero() {
		return 0, false
	}
	lasted, wasPaused = now.Sub(o.since), o.paused
	*o = writeOutage{}
	return lasted, wasPaused
}
