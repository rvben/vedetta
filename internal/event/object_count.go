package event

import "sync"

// objectCountGate keeps the retained object-count sensor agreeing with the Run
// loop's own tally.
//
// Counts change on the Run loop, in order. The publishes do not: an event's end
// publishes from a background goroutine that first waits for the event's
// snapshot and notification, so a count computed earlier can reach the broker
// after one computed later. Two ends on the same camera and label finishing
// close together reorder that way, and so does an end whose wait overlaps the
// next begin.
//
// The published value is retained, so a late publish is not a flicker. It is
// the wrong count on the operator's dashboard and in every automation reading
// that sensor until the next event on that camera and label, which for a
// driveway at night can be hours.
//
// reserve runs on the Run loop, so the sequence it hands out is the true order
// of the counts. publish then drops any value a newer one has overtaken.
type objectCountGate struct {
	mu        sync.Mutex
	next      uint64
	published map[string]uint64
}

func newObjectCountGate() *objectCountGate {
	return &objectCountGate{published: make(map[string]uint64)}
}

// reserve returns the sequence number for the next publish of a camera and
// label. Call it where the count changes, not where it is published.
func (g *objectCountGate) reserve() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.next++
	return g.next
}

// publish sends a reserved count unless a newer one has already gone out for
// the same camera and label.
//
// The decision and the handoff happen together under the gate's lock, because
// deciding first and sending afterwards leaves the same reordering one step
// later: two callers can both be admitted in order and still hand their values
// over in the opposite order. send is called with the lock held, so it must not
// block. The processor's publisher satisfies that: it copies the value onto a
// bounded queue and returns, and a worker talks to the broker.
func (g *objectCountGate) publish(cameraName, label string, seq uint64, send func()) {
	key := cameraName + "\x00" + label
	g.mu.Lock()
	defer g.mu.Unlock()
	if seq <= g.published[key] {
		return
	}
	g.published[key] = seq
	send()
}
