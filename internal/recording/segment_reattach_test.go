package recording

import (
	"context"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/media"
)

// Recording is what an NVR exists to do, and its consumer is attached once and
// then never asked about again: recordLoop used to block on ctx.Done. A
// RecordingConsumer can be detached by the source without anyone asking, because
// OnDisconnect finalizes the open segment on the fan-out goroutine and that is
// the same MP4 muxing dispatch needs a recover for. A panic there ended
// recording for the camera until the process restarted, silently: the camera
// stayed online, detection kept running, and only the missing files told.
func TestRecordingConsumerIsRebuiltAfterTheSourceDetachesIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rig := newSessionTestRig(t, ctx)
	rig.sr.reattachInterval = 5 * time.Millisecond

	rig.sr.StartRecording(ctx, "front_door", rig.url)
	if !rig.waitConsumers(t, 1, 2*time.Second) {
		t.Fatalf("recording never attached: source has %d consumers", rig.src.ConsumerCount())
	}

	rig.sr.mu.Lock()
	first := rig.sr.consumers[0]
	rig.sr.mu.Unlock()

	// What a panic inside segment finalization leaves behind.
	rig.src.RemoveConsumer(first)
	if n := rig.src.ConsumerCount(); n != 0 {
		t.Fatalf("consumer was not detached (%d attached), so there is nothing to recover from", n)
	}

	if !rig.waitConsumers(t, 1, 2*time.Second) {
		t.Fatal("the recorder never reattached: this camera records nothing until the process restarts")
	}

	rig.sr.mu.Lock()
	tracked := append([]*media.RecordingConsumer(nil), rig.sr.consumers...)
	rig.sr.mu.Unlock()
	if len(tracked) != 1 {
		t.Fatalf("recorder tracks %d consumers, want 1: the replacement was appended instead of swapped", len(tracked))
	}
	if tracked[0] == first {
		t.Error("the consumer that was detached is still the tracked one")
	}
	if !rig.src.HasConsumer(tracked[0]) {
		t.Error("the tracked consumer is not the one attached to the source, so status reports a writer that receives nothing")
	}
}

// The accepting bound. A healthy recorder must keep its consumer: rebuilding
// one closes the open segment and starts a new file, so a tick that replaced a
// working consumer would shred every recording into fragments.
func TestRecordingConsumerSurvivesTicksWhileAttached(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rig := newSessionTestRig(t, ctx)
	rig.sr.reattachInterval = 2 * time.Millisecond

	rig.sr.StartRecording(ctx, "front_door", rig.url)
	if !rig.waitConsumers(t, 1, 2*time.Second) {
		t.Fatalf("recording never attached: source has %d consumers", rig.src.ConsumerCount())
	}
	rig.sr.mu.Lock()
	first := rig.sr.consumers[0]
	rig.sr.mu.Unlock()

	// Many ticks at the interval above.
	time.Sleep(100 * time.Millisecond)

	rig.sr.mu.Lock()
	tracked := append([]*media.RecordingConsumer(nil), rig.sr.consumers...)
	rig.sr.mu.Unlock()
	if len(tracked) != 1 || tracked[0] != first {
		t.Error("an attached recording consumer was rebuilt")
	}
	if n := rig.src.ConsumerCount(); n != 1 {
		t.Errorf("source has %d consumers, want 1: a rebuild leaked a registration", n)
	}
}
