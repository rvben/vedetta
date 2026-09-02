package rtsp

import "testing"

// A consumer that a panic detached is invisible to an owner that only reads
// from it: packets stop and nothing asks why. The detach here is produced by a
// real panic through the real fan-out, not by calling RemoveConsumer, so the
// trigger this recovery exists for is proven rather than assumed.
func TestReattachRebuildsAConsumerAPanicDetached(t *testing.T) {
	silenceLogs(t)
	s := NewSource("rtsp://camera.invalid:554/stream")

	first := &panickingConsumer{panicVideo: true}
	s.AddConsumer(first)
	s.SimulateVideoRTPForTest(videoPacket(1))

	if s.HasConsumer(first) {
		t.Fatal("the source did not detach the panicking consumer, so there is nothing to recover from")
	}

	second := &panickingConsumer{}
	got, replaced := ReattachIfDetached(s, first, func() (*panickingConsumer, bool) { return second, true })
	if !replaced {
		t.Fatal("a detached consumer was not replaced, so the owner stays silent for the life of the process")
	}
	if got != second {
		t.Error("the caller was handed the detached consumer back")
	}
	if !s.HasConsumer(second) {
		t.Error("the replacement was never registered with the source")
	}
	if s.HasConsumer(first) {
		t.Error("the consumer that panicked was re-registered; its state is what panicked")
	}

	// The replacement receives what the detached one no longer could.
	s.SimulateVideoRTPForTest(videoPacket(2))
	if got := second.count(); got != 1 {
		t.Errorf("replacement received %d callbacks, want 1", got)
	}
}

// The accepting bound. Rebuilding a healthy consumer would discard a decoder's
// reference frames or a muxer's open segment every few seconds.
func TestReattachLeavesAnAttachedConsumerAlone(t *testing.T) {
	s := NewSource("rtsp://camera.invalid:554/stream")

	current := &panickingConsumer{}
	s.AddConsumer(current)

	rebuilt := false
	got, replaced := ReattachIfDetached(s, current, func() (*panickingConsumer, bool) {
		rebuilt = true
		return &panickingConsumer{}, true
	})
	if rebuilt {
		t.Error("an attached consumer was rebuilt")
	}
	if replaced || got != current {
		t.Error("an attached consumer was replaced")
	}
	if n := s.ConsumerCount(); n != 1 {
		t.Errorf("source has %d consumers, want 1", n)
	}
}

// A rebuild can fail: a decoder is unavailable, or the track is not yet known.
// The caller keeps the state it had so the next check retries, rather than
// being handed a consumer that was never attached.
func TestReattachKeepsTheOldConsumerWhenRebuildFails(t *testing.T) {
	silenceLogs(t)
	s := NewSource("rtsp://camera.invalid:554/stream")

	first := &panickingConsumer{panicVideo: true}
	s.AddConsumer(first)
	s.SimulateVideoRTPForTest(videoPacket(1))

	got, replaced := ReattachIfDetached(s, first, func() (*panickingConsumer, bool) { return nil, false })
	if replaced {
		t.Error("a failed rebuild was reported as a replacement")
	}
	if got != first {
		t.Error("a failed rebuild handed back something other than the current consumer")
	}
	if n := s.ConsumerCount(); n != 0 {
		t.Errorf("source has %d consumers after a failed rebuild, want 0", n)
	}
}
