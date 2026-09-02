package event

import "testing"

// Each camera and label carries its own retained sensor, so a person count and
// a car count on the same camera must not suppress one another. A gate keyed
// only by camera would drop every second publish on a busy camera.
func TestObjectCountGateKeepsLabelsIndependent(t *testing.T) {
	gate := newObjectCountGate()

	personSeq := gate.reserve()
	carSeq := gate.reserve()

	var sent []string
	gate.publish("front", "car", carSeq, func() { sent = append(sent, "car") })
	gate.publish("front", "person", personSeq, func() { sent = append(sent, "person") })

	if len(sent) != 2 {
		t.Fatalf("published %v, want both labels; a later car count suppressed the person count", sent)
	}
}

// The same camera and label in sequence order: both go out, because a count
// that follows another is the newer one.
func TestObjectCountGatePublishesInOrder(t *testing.T) {
	gate := newObjectCountGate()

	first := gate.reserve()
	second := gate.reserve()

	var sent []int
	gate.publish("front", "person", first, func() { sent = append(sent, 1) })
	gate.publish("front", "person", second, func() { sent = append(sent, 2) })

	if len(sent) != 2 || sent[0] != 1 || sent[1] != 2 {
		t.Fatalf("published %v, want both counts in order", sent)
	}
}

// Out of order, the overtaken count is dropped rather than sent, because the
// value is retained and would otherwise stand until the next event.
func TestObjectCountGateDropsAnOvertakenCount(t *testing.T) {
	gate := newObjectCountGate()

	stale := gate.reserve()
	current := gate.reserve()

	var sent []int
	gate.publish("front", "person", current, func() { sent = append(sent, 2) })
	gate.publish("front", "person", stale, func() { sent = append(sent, 1) })

	if len(sent) != 1 || sent[0] != 2 {
		t.Fatalf("published %v, want only the newer count", sent)
	}
}
