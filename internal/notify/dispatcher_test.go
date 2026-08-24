package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/storage"
)

// Compile-time check: *storage.DB satisfies Store. Runtime wiring happens
// in main.go; this line makes schema or interface drift fail at build time.
var _ Store = (*storage.DB)(nil)

// --- fake store implementing Store ---

type fakeStore struct {
	mu            sync.Mutex
	kv            map[string]string
	users         []string
	subs          map[string][]storage.PushSubscription // by username
	disabledPrefs map[string]bool                       // key: user|camera|class
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		kv:            map[string]string{},
		subs:          map[string][]storage.PushSubscription{},
		disabledPrefs: map[string]bool{},
	}
}

func (f *fakeStore) GetKV(k string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.kv[k]
	return v, ok, nil
}

func (f *fakeStore) SetKV(k, v string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kv[k] = v
	return nil
}

func (f *fakeStore) ListAllUsernames() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.users))
	copy(out, f.users)
	return out, nil
}

func (f *fakeStore) IsNotificationEnabled(user, camera, class string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.disabledPrefs[user+"|"+camera+"|"+class] {
		return false, nil
	}
	if f.disabledPrefs[user+"|"+camera+"|*"] {
		return false, nil
	}
	return true, nil
}

func (f *fakeStore) ListPushSubscriptionsByUser(user string) ([]storage.PushSubscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]storage.PushSubscription(nil), f.subs[user]...), nil
}

func (f *fakeStore) DeletePushSubscriptionByEndpoint(endpoint string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for u, list := range f.subs {
		out := list[:0]
		for _, s := range list {
			if s.Endpoint != endpoint {
				out = append(out, s)
			}
		}
		f.subs[u] = out
	}
	return nil
}

func (f *fakeStore) CountPushSubscriptions() (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, list := range f.subs {
		n += len(list)
	}
	return n, nil
}

// --- fake sender ---

type sendCall struct {
	Endpoint string
	Payload  []byte
}

type fakeSender struct {
	mu    sync.Mutex
	calls []sendCall
	resp  func(endpoint string, call int) SendResult
}

func (fs *fakeSender) Send(ctx context.Context, sub Subscription, payload []byte, _ *VAPID) SendResult {
	fs.mu.Lock()
	fs.calls = append(fs.calls, sendCall{Endpoint: sub.Endpoint, Payload: append([]byte(nil), payload...)})
	idx := len(fs.calls) - 1
	resp := fs.resp
	fs.mu.Unlock()
	if resp == nil {
		return SendResult{Status: 201}
	}
	return resp(sub.Endpoint, idx)
}

func (fs *fakeSender) Calls() []sendCall {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]sendCall(nil), fs.calls...)
}

// --- helpers ---

func newTestDispatcher(t *testing.T, store Store, sender Sender) *NotificationDispatcher {
	t.Helper()
	return New(Options{
		Store:          store,
		Sender:         sender,
		VAPID:          &VAPID{publicKey: "pub", privateKey: "priv"},
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		CooldownWindow: 1 * time.Minute,
		QueueCapacity:  16,
		Workers:        1,
	})
}

func seedAlice(fs *fakeStore, endpoints ...string) {
	fs.users = append(fs.users, "alice")
	for _, e := range endpoints {
		fs.subs["alice"] = append(fs.subs["alice"], storage.PushSubscription{
			ID:       int64(len(fs.subs["alice"]) + 1),
			Username: "alice",
			Endpoint: e,
			P256dh:   "p",
			Auth:     "a",
		})
	}
}

func sampleEvent() camera.Event {
	return camera.Event{
		ID:                "e1",
		CameraName:        "front",
		Label:             "person",
		Timestamp:         time.Now().UTC(),
		SnapshotAvailable: true,
	}
}

// --- tests ---

func TestDispatcher_HappyPath(t *testing.T) {
	store := newFakeStore()
	seedAlice(store, "https://push.example/a")
	sender := &fakeSender{}
	d := newTestDispatcher(t, store, sender)
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	d.Enqueue(sampleEvent())
	waitForCalls(t, sender, 1)
	cancel()
	d.Wait()

	calls := sender.Calls()
	if len(calls) != 1 || calls[0].Endpoint != "https://push.example/a" {
		t.Fatalf("unexpected calls: %+v", calls)
	}
}

func TestDispatcher_TestImageReachesEverySubscriptionOwnedByRequester(t *testing.T) {
	store := newFakeStore()
	seedAlice(store, "https://push.example/alice-phone", "https://push.example/alice-tablet")
	store.users = append(store.users, "bob")
	store.kv["notify:alice:muted"] = "1"
	store.disabledPrefs["alice|front_door|person"] = true
	store.subs["bob"] = []storage.PushSubscription{{
		ID: 3, Username: "bob", Endpoint: "https://push.example/bob-phone", P256dh: "p", Auth: "a",
	}}
	sender := &fakeSender{}
	d := New(Options{
		Store: store, Sender: sender,
		VAPID:          &VAPID{publicKey: "pub", privateKey: "priv"},
		SnapshotSigner: newTestSigner(t),
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		QueueCapacity:  16, Workers: 1, CooldownWindow: time.Minute,
	})
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	d.EnqueueTest("alice", "front_door", "front-latest")
	waitForCalls(t, sender, 2)
	cancel()
	d.Wait()

	calls := sender.Calls()
	if len(calls) != 2 {
		t.Fatalf("test sends = %d, want both of Alice's subscriptions", len(calls))
	}
	for _, call := range calls {
		if call.Endpoint == "https://push.example/bob-phone" {
			t.Fatal("test notification leaked to another user's subscription")
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(call.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		image, _ := payload["image"].(string)
		if !strings.HasPrefix(image, "/api/push/snapshot/front-latest?") {
			t.Errorf("subscription %s image = %q", call.Endpoint, image)
		}
	}
}

func TestDispatcher_Muted(t *testing.T) {
	store := newFakeStore()
	seedAlice(store, "https://push.example/a")
	_ = store.SetKV("notify:alice:muted", "1")
	sender := &fakeSender{}
	d := newTestDispatcher(t, store, sender)
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	d.Enqueue(sampleEvent())
	time.Sleep(50 * time.Millisecond)
	cancel()
	d.Wait()
	if n := len(sender.Calls()); n != 0 {
		t.Fatalf("muted user got %d sends", n)
	}
	if d.metrics.EventsMuted.Load() != 1 {
		t.Fatalf("EventsMuted = %d", d.metrics.EventsMuted.Load())
	}
}

func TestDispatcher_WildcardDisable(t *testing.T) {
	store := newFakeStore()
	seedAlice(store, "https://push.example/a")
	store.disabledPrefs["alice|front|*"] = true
	sender := &fakeSender{}
	d := newTestDispatcher(t, store, sender)
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	d.Enqueue(sampleEvent())
	time.Sleep(50 * time.Millisecond)
	cancel()
	d.Wait()
	if n := len(sender.Calls()); n != 0 {
		t.Fatalf("wildcard-disabled user got %d sends", n)
	}
}

func TestDispatcher_CooldownSuppression(t *testing.T) {
	store := newFakeStore()
	seedAlice(store, "https://push.example/a")
	sender := &fakeSender{}
	d := newTestDispatcher(t, store, sender)
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	d.Enqueue(sampleEvent())
	waitForCalls(t, sender, 1)
	d.Enqueue(sampleEvent())
	time.Sleep(50 * time.Millisecond)
	cancel()
	d.Wait()
	if n := len(sender.Calls()); n != 1 {
		t.Fatalf("cooldown should suppress second event, got %d sends", n)
	}
	if d.metrics.EventsCooldown.Load() != 1 {
		t.Fatalf("EventsCooldown = %d", d.metrics.EventsCooldown.Load())
	}
}

func TestDispatcher_SeparateActivitiesBypassEventCooldown(t *testing.T) {
	store := newFakeStore()
	seedAlice(store, "https://push.example/a")
	sender := &fakeSender{}
	d := newTestDispatcher(t, store, sender)
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	base := sampleEvent()
	first := storage.Activity{ID: "act_1", CameraName: base.CameraName, StartTime: base.Timestamp,
		EventCount: 1, Labels: []string{base.Label}, PrimaryEvent: base}
	second := first
	second.ID = "act_2"
	second.PrimaryEvent.ID = "e2"
	if !d.EnqueueActivity(first) {
		t.Fatal("first activity was not accepted")
	}
	waitForCalls(t, sender, 1)
	if !d.EnqueueActivity(second) {
		t.Fatal("second activity was not accepted")
	}
	waitForCalls(t, sender, 2)
	cancel()
	d.Wait()
	if d.metrics.EventsCooldown.Load() != 0 {
		t.Fatalf("separate activities shared a cooldown")
	}
}

func TestDispatcher_CooldownOnlyOnSuccess(t *testing.T) {
	store := newFakeStore()
	seedAlice(store, "https://push.example/a")
	sender := &fakeSender{resp: func(_ string, _ int) SendResult { return SendResult{Status: 500} }}
	d := newTestDispatcher(t, store, sender)
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	d.Enqueue(sampleEvent())
	waitForCalls(t, sender, 1)
	d.Enqueue(sampleEvent())
	waitForCalls(t, sender, 2)
	cancel()
	d.Wait()
	// Cooldown should NOT have been marked (no success) → second event was dispatched.
	if d.metrics.EventsCooldown.Load() != 0 {
		t.Fatalf("expected no cooldown suppression, got %d", d.metrics.EventsCooldown.Load())
	}
}

func TestDispatcher_ZeroSubsDoesNotMarkCooldown(t *testing.T) {
	store := newFakeStore()
	store.users = append(store.users, "alice") // no subs
	sender := &fakeSender{}
	d := newTestDispatcher(t, store, sender)
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	d.Enqueue(sampleEvent())
	d.Enqueue(sampleEvent())
	time.Sleep(50 * time.Millisecond)
	cancel()
	d.Wait()
	if d.metrics.EventsCooldown.Load() != 0 {
		t.Fatalf("zero subs should not mark cooldown, got suppression count %d", d.metrics.EventsCooldown.Load())
	}
}

func TestDispatcher_410PrunesSubscription(t *testing.T) {
	store := newFakeStore()
	seedAlice(store, "https://push.example/a", "https://push.example/b")
	sender := &fakeSender{resp: func(ep string, _ int) SendResult {
		if ep == "https://push.example/a" {
			return SendResult{Status: 410}
		}
		return SendResult{Status: 201}
	}}
	d := newTestDispatcher(t, store, sender)
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	d.Enqueue(sampleEvent())
	waitForCalls(t, sender, 2)
	time.Sleep(20 * time.Millisecond)
	cancel()
	d.Wait()

	subs, _ := store.ListPushSubscriptionsByUser("alice")
	if len(subs) != 1 || subs[0].Endpoint != "https://push.example/b" {
		t.Fatalf("expected /a to be pruned, got %+v", subs)
	}
}

func TestDispatcher_PanicRecovery(t *testing.T) {
	store := newFakeStore()
	seedAlice(store, "https://push.example/a")
	sender := &fakeSender{resp: func(ep string, idx int) SendResult {
		if idx == 0 {
			panic("boom")
		}
		return SendResult{Status: 201}
	}}
	d := newTestDispatcher(t, store, sender)
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	d.Enqueue(sampleEvent())
	time.Sleep(50 * time.Millisecond)
	// Second event should still go through.
	d.Enqueue(sampleEvent())
	waitForCalls(t, sender, 2)
	cancel()
	d.Wait()
}

func TestDispatcher_QueueOverflowDrops(t *testing.T) {
	store := newFakeStore()
	seedAlice(store, "https://push.example/a")
	blockCh := make(chan struct{})
	sender := &fakeSender{resp: func(_ string, _ int) SendResult {
		<-blockCh
		return SendResult{Status: 201}
	}}
	d := New(Options{
		Store:          store,
		Sender:         sender,
		VAPID:          &VAPID{publicKey: "p", privateKey: "k"},
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		QueueCapacity:  2,
		Workers:        1,
		CooldownWindow: time.Minute,
	})
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	// Block the single worker on the first event.
	d.Enqueue(sampleEvent())
	// Now cram the queue.
	for i := 0; i < 10; i++ {
		d.Enqueue(sampleEvent())
	}
	if d.metrics.EventsDropped.Load() == 0 {
		t.Fatalf("expected drops, got 0")
	}
	close(blockCh)
	cancel()
	d.Wait()
}

func TestDispatcher_TimeoutDoesNotPrune(t *testing.T) {
	store := newFakeStore()
	seedAlice(store, "https://push.example/a")
	sender := &fakeSender{resp: func(_ string, _ int) SendResult {
		return SendResult{Status: 0, Err: errors.New("ctx deadline"), Timeout: true}
	}}
	d := newTestDispatcher(t, store, sender)
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	d.Enqueue(sampleEvent())
	waitForCalls(t, sender, 1)
	cancel()
	d.Wait()
	subs, _ := store.ListPushSubscriptionsByUser("alice")
	if len(subs) != 1 {
		t.Fatalf("timeout must not prune subscription, got %d", len(subs))
	}
}

// --- utilities ---

func waitForCalls(t *testing.T, fs *fakeSender, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(fs.Calls()) >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d sends, got %d", want, len(fs.Calls()))
}
