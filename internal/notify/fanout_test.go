package notify

import (
	"testing"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/storage"
)

type testProvider struct {
	accept bool
	rings  int
	acts   int
}

func (*testProvider) Enqueue(camera.Event)                    {}
func (p *testProvider) EnqueueDoorbell(camera.Event) bool     { p.rings++; return p.accept }
func (p *testProvider) EnqueueActivity(storage.Activity) bool { p.acts++; return p.accept }

func TestFanoutProviderFailureIsIsolated(t *testing.T) {
	full, ready := &testProvider{}, &testProvider{accept: true}
	f := NewFanout(full, ready)
	if !f.EnqueueDoorbell(pushoverRing()) || full.rings != 1 || ready.rings != 1 {
		t.Fatal("full provider must not suppress another provider's ring")
	}
	a := storage.Activity{ID: "activity"}
	if f.EnqueueActivity(a) || full.acts != 1 || ready.acts != 1 {
		t.Fatal("activity should await the provider whose queue is full")
	}
	full.accept = true
	if !f.EnqueueActivity(a) || full.acts != 2 || ready.acts != 1 {
		t.Fatal("retry duplicated the already accepted notification")
	}
	if len(f.pending) != 0 {
		t.Fatal("completed fanout leaked pending state")
	}
}

type earlyTestProvider struct{ testProvider }

func (p *earlyTestProvider) EnqueueDoorbellEarly(ev camera.Event) func(camera.Event) bool {
	if p.EnqueueDoorbell(ev) {
		return nil
	}
	return p.EnqueueDoorbell
}
func TestFanoutEarlySuccessIsNotRepeatedAfterStorage(t *testing.T) {
	early := &earlyTestProvider{testProvider{accept: true}}
	full := &earlyTestProvider{}
	web := &testProvider{accept: true}
	after := NewFanout(early, full, web).EnqueueDoorbellEarly(pushoverRing())
	if after == nil || early.rings != 1 || full.rings != 1 || web.rings != 0 {
		t.Fatal("incorrect early delivery plan")
	}
	full.accept = true
	if !after(pushoverRing()) || early.rings != 1 || full.rings != 2 || web.rings != 1 {
		t.Fatal("late phase duplicated or skipped a provider")
	}
}
