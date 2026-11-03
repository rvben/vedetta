package notify

import (
	"sync"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/storage"
)

// Provider accepts notification jobs without blocking on network delivery.
type Provider interface {
	Enqueue(camera.Event)
	EnqueueDoorbell(camera.Event) bool
	EnqueueActivity(storage.Activity) bool
}

// Fanout connects independent providers to the event processor. Each provider
// keeps its own queue, credentials, filters, and delivery failures.
type Fanout struct {
	providers []Provider
	mu        sync.Mutex
	pending   map[string][]bool
}

func NewFanout(providers ...Provider) *Fanout {
	return &Fanout{providers: providers, pending: make(map[string][]bool)}
}

func (f *Fanout) Enqueue(ev camera.Event) {
	for _, p := range f.providers {
		p.Enqueue(ev)
	}
}

func (f *Fanout) EnqueueCry(ev camera.Event) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := "cry:" + ev.ID
	accepted := f.pending[key]
	if accepted == nil {
		accepted = make([]bool, len(f.providers))
	}
	complete := true
	for i, p := range f.providers {
		if !accepted[i] {
			if immediate, ok := p.(interface{ EnqueueCry(camera.Event) bool }); ok {
				accepted[i] = immediate.EnqueueCry(ev)
			} else {
				accepted[i] = true
			}
		}
		complete = complete && accepted[i]
	}
	if complete {
		delete(f.pending, key)
	} else {
		f.pending[key] = accepted
	}
	return complete
}

func (f *Fanout) ReleaseCry(id string) { f.mu.Lock(); delete(f.pending, "cry:"+id); f.mu.Unlock() }

func (f *Fanout) EnqueueCryEarly(ev camera.Event) func(camera.Event) bool {
	var remaining []func(camera.Event) bool
	for _, p := range f.providers {
		if early, ok := p.(interface {
			EnqueueCryEarly(camera.Event) func(camera.Event) bool
		}); ok {
			if after := early.EnqueueCryEarly(ev); after != nil {
				remaining = append(remaining, after)
			}
		} else if immediate, ok := p.(interface{ EnqueueCry(camera.Event) bool }); ok {
			remaining = append(remaining, immediate.EnqueueCry)
		}
	}
	if len(remaining) == 0 {
		return nil
	}
	return func(saved camera.Event) bool {
		complete := true
		for i, enqueue := range remaining {
			if enqueue != nil {
				if enqueue(saved) {
					remaining[i] = nil
				} else {
					complete = false
				}
			}
		}
		return complete
	}
}

func (f *Fanout) EnqueueDoorbell(ev camera.Event) bool {
	accepted := false
	for _, p := range f.providers {
		if p.EnqueueDoorbell(ev) {
			accepted = true
		}
	}
	return accepted
}

func (f *Fanout) EnqueueActivity(activity storage.Activity) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	accepted, exists := f.pending[activity.ID]
	if !exists {
		accepted = make([]bool, len(f.providers))
	}
	complete := true
	for i, p := range f.providers {
		if !accepted[i] {
			accepted[i] = p.EnqueueActivity(activity)
		}
		complete = complete && accepted[i]
	}
	if complete {
		delete(f.pending, activity.ID)
	} else {
		// The processor retries its first rejected Activity before advancing.
		// Remember successful providers so a full queue cannot duplicate pushes
		// through another provider on that retry.
		f.pending[activity.ID] = accepted
	}
	return complete
}

// EnqueueDoorbellEarly gives inline-photo providers a head start. The remainder
// is kept in this event's closure, so partial acceptance neither duplicates a
// successful push nor creates persistent per-event bookkeeping.
func (f *Fanout) EnqueueDoorbellEarly(ev camera.Event) func(camera.Event) bool {
	var remaining []func(camera.Event) bool
	for _, p := range f.providers {
		if early, ok := p.(interface {
			EnqueueDoorbellEarly(camera.Event) func(camera.Event) bool
		}); ok {
			if after := early.EnqueueDoorbellEarly(ev); after != nil {
				remaining = append(remaining, after)
			}
		} else {
			remaining = append(remaining, p.EnqueueDoorbell)
		}
	}
	if len(remaining) == 0 {
		return nil
	}
	return func(saved camera.Event) bool {
		accepted := false
		for _, enqueue := range remaining {
			if enqueue(saved) {
				accepted = true
			}
		}
		return accepted
	}
}
