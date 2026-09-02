package event

import (
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/camera"
)

func newBareProcessor() *Processor {
	return &Processor{pendingEnds: make(map[string]pendingEnd)}
}

// A camera emitting ends for events that never begin must not grow the parked
// map without limit.
func TestParkEndIsCapped(t *testing.T) {
	p := newBareProcessor()
	for i := range maxPendingEnds + 50 {
		p.parkEnd(camera.EventEnd{
			EventID:    string(rune('a')) + time.Duration(i).String(),
			CameraName: "front",
			EndTime:    time.Now(),
		})
	}
	if got := len(p.pendingEnds); got > maxPendingEnds {
		t.Fatalf("parked %d ends, want at most %d", got, maxPendingEnds)
	}
}

// The oldest parked end is the one evicted, so a recent end still has a chance
// of being adopted by its event.
func TestParkEndEvictsTheOldest(t *testing.T) {
	p := newBareProcessor()
	base := time.Now()
	for i := range maxPendingEnds {
		p.pendingEnds[string(rune(i))+"-old"] = pendingEnd{
			endTime:  base,
			parkedAt: base.Add(time.Duration(i) * time.Millisecond),
		}
	}
	oldest := string(rune(0)) + "-old"
	p.parkEnd(camera.EventEnd{EventID: "newcomer", CameraName: "front", EndTime: base})

	if _, still := p.pendingEnds[oldest]; still {
		t.Error("the oldest parked end survived eviction")
	}
	if _, added := p.pendingEnds["newcomer"]; !added {
		t.Error("the new end was not parked")
	}
}

// An end whose event never arrives is a genuinely lost end. It is dropped after
// the retention window rather than held forever.
func TestExpirePendingEndsDropsOnlyStaleEntries(t *testing.T) {
	p := newBareProcessor()
	now := time.Now()
	p.pendingEnds["stale"] = pendingEnd{endTime: now, parkedAt: now.Add(-pendingEndTTL - time.Second)}
	p.pendingEnds["fresh"] = pendingEnd{endTime: now, parkedAt: now.Add(-time.Second)}

	p.expirePendingEnds(now)

	if _, still := p.pendingEnds["stale"]; still {
		t.Error("a stale parked end was kept")
	}
	if _, still := p.pendingEnds["fresh"]; !still {
		t.Error("a fresh parked end was dropped")
	}
}
