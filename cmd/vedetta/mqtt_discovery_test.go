package main

import (
	"sync"
	"testing"
)

// fakeConnectHook stands in for an MQTT client. Constructing a real one
// requires a broker, and what matters here is only that the announcer
// registers a reconnect callback and can fire it.
type fakeConnectHook struct {
	mu        sync.Mutex
	onConnect func()
}

func (h *fakeConnectHook) SetOnConnect(fn func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onConnect = fn
}

// reconnect simulates the broker connection coming back.
func (h *fakeConnectHook) reconnect(t *testing.T) {
	t.Helper()
	h.mu.Lock()
	fn := h.onConnect
	h.mu.Unlock()
	if fn == nil {
		t.Fatal("no reconnect callback was registered on the client")
	}
	fn()
}

// Home Assistant discovery is retained broker state. A broker that restarts
// drops it, so every reconnect has to publish it again or the entities stay
// missing until the process is restarted.
func TestAnnouncerRepublishesOnEveryReconnect(t *testing.T) {
	var announcer mqttAnnouncer
	client := &fakeConnectHook{}
	calls := 0

	announcer.setAnnounce(func() { calls++ })
	announcer.attach(client)
	if calls != 1 {
		t.Fatalf("announcements after attach = %d, want 1", calls)
	}

	client.reconnect(t)
	client.reconnect(t)
	if calls != 3 {
		t.Fatalf("announcements after two reconnects = %d, want 3", calls)
	}
}

// The broker can be down at startup, in which case the background reconnect
// installs a client long after the announcement was configured. That client
// must still be announced to.
func TestAnnouncerAnnouncesToALateClient(t *testing.T) {
	var announcer mqttAnnouncer
	calls := 0
	announcer.setAnnounce(func() { calls++ })
	if calls != 0 {
		t.Fatalf("announced with no client attached, calls = %d", calls)
	}

	announcer.attach(&fakeConnectHook{})
	if calls != 1 {
		t.Fatalf("announcements for a late client = %d, want 1", calls)
	}
}

// The background reconnect can win the race and attach a client before startup
// has finished configuring the announcement. The announcement must not be lost.
func TestAnnouncerAnnouncesWhenTheClientArrivesFirst(t *testing.T) {
	var announcer mqttAnnouncer
	client := &fakeConnectHook{}
	announcer.attach(client)

	calls := 0
	announcer.setAnnounce(func() { calls++ })
	if calls != 1 {
		t.Fatalf("announcements after a late setAnnounce = %d, want 1", calls)
	}

	client.reconnect(t)
	if calls != 2 {
		t.Fatalf("announcements after reconnect = %d, want 2", calls)
	}
}

// A nil client is what the startup path holds while the broker is unreachable.
// It must not be attached, and it must not panic.
func TestAnnouncerIgnoresANilClient(t *testing.T) {
	var announcer mqttAnnouncer
	announcer.attach(nil)
	calls := 0
	announcer.setAnnounce(func() { calls++ })
	if calls != 0 {
		t.Fatalf("announced to a nil client, calls = %d", calls)
	}
}
