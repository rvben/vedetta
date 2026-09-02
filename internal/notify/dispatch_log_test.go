package notify

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// --- log capture ---

type capturedRecord struct {
	Level slog.Level
	Msg   string
	Attrs map[string]any
}

// logCapture is a slog.Handler that keeps every record in memory so tests can
// assert on what an operator would actually read in the log.
type logCapture struct {
	mu      sync.Mutex
	records []capturedRecord
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	rec := capturedRecord{Level: r.Level, Msg: r.Message, Attrs: map[string]any{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.Attrs[a.Key] = a.Value.Any()
		return true
	})
	c.mu.Lock()
	c.records = append(c.records, rec)
	c.mu.Unlock()
	return nil
}

func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

func (c *logCapture) withMessage(msg string) []capturedRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []capturedRecord
	for _, r := range c.records {
		if r.Msg == msg {
			out = append(out, r)
		}
	}
	return out
}

// waitForRecords blocks until want records carrying msg have been logged.
func (c *logCapture) waitForRecords(t *testing.T, msg string, want int) []capturedRecord {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := c.withMessage(msg); len(got) >= want {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := c.withMessage(msg)
	t.Fatalf("timed out waiting for %d %q records, got %d: %+v", want, msg, len(got), got)
	return nil
}

func newLoggingDispatcher(t *testing.T, store Store, sender Sender) (*NotificationDispatcher, *logCapture) {
	t.Helper()
	capture := &logCapture{}
	d := New(Options{
		Store:          store,
		Sender:         sender,
		VAPID:          &VAPID{publicKey: "pub", privateKey: "priv"},
		Logger:         slog.New(capture),
		CooldownWindow: 1 * time.Minute,
		QueueCapacity:  16,
		Workers:        1,
	})
	return d, capture
}

// attrInt reads a numeric attribute. slog stores Go ints as int64, so tests
// must not compare against an untyped int directly.
func attrInt(t *testing.T, rec capturedRecord, key string) int64 {
	t.Helper()
	v, ok := rec.Attrs[key]
	if !ok {
		t.Fatalf("record %+v has no %q attribute", rec.Attrs, key)
	}
	n, ok := v.(int64)
	if !ok {
		t.Fatalf("attribute %q = %#v, want an integer", key, v)
	}
	return n
}

func assertNoAttr(t *testing.T, rec capturedRecord, key string) {
	t.Helper()
	if v, ok := rec.Attrs[key]; ok {
		t.Fatalf("record carries unexpected attribute %q = %#v", key, v)
	}
}

// --- tests ---

// A successful fan-out must report how many users it actually reached, so a
// delivery can be confirmed from the log alone.
func TestDispatchLog_ReportsDeliveryCount(t *testing.T) {
	store := newFakeStore()
	seedAlice(store, "https://push.example/a")
	sender := &fakeSender{}
	d, capture := newLoggingDispatcher(t, store, sender)
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	d.Enqueue(sampleEvent())
	waitForCalls(t, sender, 1)
	records := capture.waitForRecords(t, "push dispatch", 1)
	cancel()
	d.Wait()

	if len(records) != 1 {
		t.Fatalf("one event logged %d dispatch records", len(records))
	}
	if n := attrInt(t, records[0], "delivered"); n != 1 {
		t.Errorf("delivered = %d, want 1", n)
	}
	if records[0].Level != slog.LevelInfo {
		t.Errorf("successful dispatch logged at %v, want INFO", records[0].Level)
	}
}

// The defect this guards: a muted account produced a dispatch line indicating
// a healthy fan-out, so seven days of total notification silence read exactly
// like seven days of successful delivery.
func TestDispatchLog_MutedUserIsReportedAsUndelivered(t *testing.T) {
	store := newFakeStore()
	seedAlice(store, "https://push.example/a")
	if err := store.SetKV("notify:alice:muted", "1"); err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{}
	d, capture := newLoggingDispatcher(t, store, sender)
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	d.Enqueue(sampleEvent())
	records := capture.waitForRecords(t, "push dispatch", 1)
	cancel()
	d.Wait()

	if n := attrInt(t, records[0], "delivered"); n != 0 {
		t.Errorf("delivered = %d, want 0 for a muted account", n)
	}
	if n := attrInt(t, records[0], "muted"); n != 1 {
		t.Errorf("muted = %d, want 1", n)
	}
}

// Reasons a dispatch did not deliver are named individually, so an operator
// can tell a muted account from a disabled preference or a device that has
// never subscribed without correlating against /metrics.
func TestDispatchLog_NamesEachDropReason(t *testing.T) {
	store := newFakeStore()
	seedAlice(store, "https://push.example/a")
	store.users = append(store.users, "bob", "carol")
	store.subs["bob"] = store.subs["alice"]
	// carol has no subscription at all.
	if err := store.SetKV("notify:alice:muted", "1"); err != nil {
		t.Fatal(err)
	}
	store.disabledPrefs["bob|front|person"] = true
	sender := &fakeSender{}
	d, capture := newLoggingDispatcher(t, store, sender)
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	d.Enqueue(sampleEvent())
	records := capture.waitForRecords(t, "push dispatch", 1)
	cancel()
	d.Wait()

	rec := records[0]
	if n := attrInt(t, rec, "delivered"); n != 0 {
		t.Errorf("delivered = %d, want 0", n)
	}
	for key, want := range map[string]int64{
		"muted":            1,
		"disabled":         1,
		"no_subscriptions": 1,
	} {
		if n := attrInt(t, rec, key); n != want {
			t.Errorf("%s = %d, want %d", key, n, want)
		}
	}
	// Reasons that did not occur stay off the line entirely; a log of
	// permanent zeroes is as unreadable as no log at all.
	assertNoAttr(t, rec, "cooldown")
	assertNoAttr(t, rec, "send_failed")
}

// Zero deliveries is a fault only while the process has never delivered
// anything. That condition cannot arise from ordinary suppression, because
// every suppressing gate requires a prior successful send to arm it.
func TestDispatchLog_WarnsWhileNothingHasEverBeenDelivered(t *testing.T) {
	store := newFakeStore()
	seedAlice(store, "https://push.example/a")
	if err := store.SetKV("notify:alice:muted", "1"); err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{}
	d, capture := newLoggingDispatcher(t, store, sender)
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	d.Enqueue(sampleEvent())
	records := capture.waitForRecords(t, "push dispatch", 1)
	cancel()
	d.Wait()

	if records[0].Level != slog.LevelWarn {
		t.Fatalf("dispatch that has never delivered logged at %v, want WARN", records[0].Level)
	}
}

// Negative control for the rule above: once delivery is proven to work, a
// suppressed event is routine and must not be escalated.
func TestDispatchLog_SuppressionAfterADeliveryStaysInfo(t *testing.T) {
	store := newFakeStore()
	seedAlice(store, "https://push.example/a")
	sender := &fakeSender{}
	d, capture := newLoggingDispatcher(t, store, sender)
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	d.Enqueue(sampleEvent())
	waitForCalls(t, sender, 1)
	d.Enqueue(sampleEvent())
	records := capture.waitForRecords(t, "push dispatch", 2)
	cancel()
	d.Wait()

	suppressed := records[1]
	if n := attrInt(t, suppressed, "cooldown"); n != 1 {
		t.Fatalf("cooldown = %d, want 1", n)
	}
	if suppressed.Level != slog.LevelInfo {
		t.Errorf("suppression after a proven delivery logged at %v, want INFO", suppressed.Level)
	}
}

// A delivery test bypasses every gate, so its outcome line reports transport
// results only. It still has to say whether the devices were reached.
func TestDispatchLog_TestNotificationReportsDelivery(t *testing.T) {
	store := newFakeStore()
	seedAlice(store, "https://push.example/a", "https://push.example/b")
	sender := &fakeSender{}
	d, capture := newLoggingDispatcher(t, store, sender)
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	d.EnqueueTest("alice", "front", "")
	waitForCalls(t, sender, 2)
	records := capture.waitForRecords(t, "push dispatch", 1)
	cancel()
	d.Wait()

	if n := attrInt(t, records[0], "delivered"); n != 1 {
		t.Errorf("delivered = %d, want 1 (one user, both devices)", n)
	}
}

// A push service rejecting every endpoint is a transport fault, distinct from
// a deliberate suppression, and must be named as such.
func TestDispatchLog_SendFailureIsNamed(t *testing.T) {
	store := newFakeStore()
	seedAlice(store, "https://push.example/a")
	sender := &fakeSender{resp: func(string, int) SendResult { return SendResult{Status: 500} }}
	d, capture := newLoggingDispatcher(t, store, sender)
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	d.Enqueue(sampleEvent())
	waitForCalls(t, sender, 1)
	records := capture.waitForRecords(t, "push dispatch", 1)
	cancel()
	d.Wait()

	if n := attrInt(t, records[0], "send_failed"); n != 1 {
		t.Errorf("send_failed = %d, want 1", n)
	}
	assertNoAttr(t, records[0], "muted")
}
