package event_test

import (
	"sync"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/config"
	eventprocessor "github.com/rvben/vedetta/internal/event"
)

// countPublisher records object counts in the order the broker would see them.
// Everything else is a no-op: this test is about one retained value.
type countPublisher struct {
	mu     sync.Mutex
	counts []int
}

func (p *countPublisher) PublishEvent(camera.Event, []string) error  { return nil }
func (p *countPublisher) PublishSnapshot(string, string, []byte)     {}
func (p *countPublisher) PublishDoorbell(string, string, []byte)     {}
func (p *countPublisher) PublishObjectSighting(string, camera.Event) {}
func (p *countPublisher) PublishPresence(camera.PresenceEvent, string) error {
	return nil
}

func (p *countPublisher) PublishObjectCount(_, _ string, count int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.counts = append(p.counts, count)
	return nil
}

func (p *countPublisher) last() (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.counts) == 0 {
		return 0, false
	}
	return p.counts[len(p.counts)-1], true
}

func (p *countPublisher) snapshot() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.counts...)
}

// An event's end publishes its object count from a background goroutine that
// first waits for the event's snapshot and notification. That wait can outlast
// the next event on the same camera and label, so the count the end computed is
// no longer current by the time it would be sent. The count is published
// retained, so sending it leaves the operator's dashboard and every automation
// reading that sensor showing an object that is not there until the next event,
// which for an overnight camera is hours.
func TestProcessorStaleObjectCountFromASlowEndIsNotPublished(t *testing.T) {
	cfg := &config.Config{}
	cfg.Recording.MaxEventDuration = time.Hour
	notifier := newBlockingNotifier()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(notifier.release) }) }
	defer release()
	publisher := &countPublisher{}

	fixture := newRunningProcessor(t, cfg, func(options *eventprocessor.Options) {
		options.Notifier = notifier
		options.Publisher = func() eventprocessor.Publisher { return publisher }
	})

	started := time.Now().Add(-10 * time.Second).UTC().Truncate(time.Millisecond)
	fixture.events <- camera.Event{
		ID: "first", CameraName: "front", Label: "person",
		Category: camera.CategoryAlert, Timestamp: started,
	}
	waitForStoredEvent(t, fixture.db, "first", func(*camera.Event) bool { return true })

	select {
	case <-notifier.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("notifier was never called, the test cannot stall the end's publish")
	}

	// The end drops the count to zero and then parks behind the stalled emit.
	fixture.ends <- camera.EventEnd{
		EventID: "first", CameraName: "front", EndTime: started.Add(2 * time.Second),
	}
	waitForDrainedEnds(t, fixture.ends)

	// A second person walks in before that publish gets out. The count is one
	// again, and one is what the sensor must be left showing.
	fixture.events <- camera.Event{
		ID: "second", CameraName: "front", Label: "person",
		Category: camera.CategoryAlert, Timestamp: time.Now(),
	}
	waitForStoredEvent(t, fixture.db, "second", func(*camera.Event) bool { return true })

	// Releasing the notifier lets the parked end finish its wait and try to
	// publish the count it computed before the second event existed.
	release()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got, ok := publisher.last(); ok && got == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Give any stale publish the same window to arrive as the good one had.
	time.Sleep(200 * time.Millisecond)

	got, ok := publisher.last()
	if !ok {
		t.Fatal("no object count was published at all")
	}
	if got != 1 {
		t.Fatalf("retained object count is %d, want 1; published sequence %v", got, publisher.snapshot())
	}
}
