package rtsp

import "time"

// ReattachInterval is how often an owner should check that a consumer it
// registered is still attached.
//
// The check is a slice scan under a read lock, so the interval is set by how
// long an outage is worth tolerating rather than by what the check costs. Five
// seconds is below the threshold at which a camera is reported offline, so a
// recovery never has to be explained to an operator.
const ReattachInterval = 5 * time.Second

// ReattachIfDetached re-registers a consumer that the Source dropped, returning
// the consumer to keep using and whether it is a new one.
//
// A Source detaches a consumer whose callback panicked (see deliver): its state
// after a panic is unknown, so dropping it is safer than feeding it more
// packets. That recovery is only half of the mechanism. It assumes the owner
// notices and reattaches, and an owner that attaches once at startup and then
// only reads from the consumer never asks again: packets simply stop, and a
// single malformed access unit ends recording or detection for that camera
// until the process restarts.
//
// A fresh consumer is built rather than the old one re-registered, since the
// old one's decoder or muxer state is exactly what the panic happened in.
// rebuild reports false when it cannot produce one, which leaves the caller's
// state untouched so the next check tries again.
func ReattachIfDetached[T Consumer](source *Source, current T, rebuild func() (T, bool)) (T, bool) {
	if source == nil || source.HasConsumer(current) {
		return current, false
	}
	next, ok := rebuild()
	if !ok {
		return current, false
	}
	source.AddConsumer(next)
	return next, true
}
