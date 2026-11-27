package notify

import (
	"sync"
	"time"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/cry"
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
	cryStatus map[string]*cryStatusDelivery
}

func NewFanout(providers ...Provider) *Fanout {
	return &Fanout{providers: providers, pending: make(map[string][]bool), cryStatus: make(map[string]*cryStatusDelivery)}
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

// cryStatusProvider is a provider that carries crying recognition notices.
type cryStatusProvider interface {
	EnqueueCryStatus(cry.OutageNotice) bool
}

// cryStatusFilter is a provider that sends a camera's crying recognition
// notices to nobody when its settings leave that camera out. It accepts such
// a notice, since there is nothing to retry.
type cryStatusFilter interface {
	DeliversCryStatus(camera string) bool
}

// cryStatusDelivery is the latest crying recognition notice of one camera
// that some provider has yet to take.
type cryStatusDelivery struct {
	recovered bool
	since     time.Time
	accepted  []bool
}

func (d *cryStatusDelivery) of(n cry.OutageNotice) bool {
	return d != nil && d.recovered == n.Recovered && d.since.Equal(n.Since)
}

// EnqueueCryStatus hands a crying recognition notice to every provider that
// carries them and reports whether all of them took it. Providers that already
// accepted are remembered, so the caller's retry does not send it twice. A
// camera's newer notice ends the retries of its previous one: a provider that
// never took an outage gets the recovery alone.
func (f *Fanout) EnqueueCryStatus(n cry.OutageNotice) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.cryStatus[n.Camera]
	if !d.of(n) {
		d = &cryStatusDelivery{recovered: n.Recovered, since: n.Since, accepted: make([]bool, len(f.providers))}
	}
	complete := true
	for i, p := range f.providers {
		if !d.accepted[i] {
			if status, ok := p.(cryStatusProvider); ok {
				d.accepted[i] = status.EnqueueCryStatus(n)
			} else {
				d.accepted[i] = true
			}
		}
		complete = complete && d.accepted[i]
	}
	if complete {
		delete(f.cryStatus, n.Camera)
	} else {
		f.cryStatus[n.Camera] = d
	}
	return complete
}

// CryStatusReached reports whether a notice that EnqueueCryStatus could not
// hand to every provider was taken by one that delivers it to someone. People
// have then been told, although not all of them.
func (f *Fanout) CryStatusReached(n cry.OutageNotice) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.cryStatus[n.Camera]
	if !d.of(n) {
		return false
	}
	for i, p := range f.providers {
		if _, carries := p.(cryStatusProvider); !carries || !d.accepted[i] {
			continue
		}
		if filter, ok := p.(cryStatusFilter); ok && !filter.DeliversCryStatus(n.Camera) {
			continue
		}
		return true
	}
	return false
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
