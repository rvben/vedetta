package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/storage"
)

// Sender is the narrow interface over the webpush library. Tests inject a
// fake so dispatcher_test.go stays hermetic; production wires WebPushSender.
type Sender interface {
	Send(ctx context.Context, sub Subscription, payload []byte, vapid *VAPID) SendResult
}

// Subscription is the subset of storage.PushSubscription the sender needs.
type Subscription struct {
	Endpoint string
	P256dh   string
	Auth     string
}

// SendResult categorizes the outcome of a single Send call. Status is the
// HTTP status code from the push service, 0 on transport/timeout error.
// Body holds the response body on non-2xx responses, truncated to 4KB,
// so the dispatcher can surface push service error messages in logs.
type SendResult struct {
	Status  int
	Err     error
	Timeout bool
	Body    string
}

// NotificationDispatcher consumes confirmed-track events and fans out web
// push notifications. Callers call Start once and Enqueue per event. The
// Enqueue path is non-blocking — the detection hot path must never stall.
type NotificationDispatcher struct {
	store          Store
	sender         Sender
	vapid          *VAPID
	snapshotSigner *SnapshotSigner
	cooldown       *CooldownCache
	backoff        *CooldownCache
	metrics        *Metrics
	logger         *slog.Logger
	window         time.Duration
	jobs           chan notificationJob
	workers        int
	wg             sync.WaitGroup
}

type notificationJob struct {
	event           camera.Event
	activity        *storage.Activity
	immediate       bool
	test            bool
	targetUser      string
	snapshotEventID string
}

// dropReason names why one user's notification was not delivered. The empty
// value means delivered. Each reason appears verbatim as an attribute on the
// dispatch log line, so an operator reads the same word the code branched on.
type dropReason string

const (
	dropDelivered       dropReason = ""
	dropMuted           dropReason = "muted"
	dropDisabled        dropReason = "disabled"
	dropCooldown        dropReason = "cooldown"
	dropNoSubs          dropReason = "no_subscriptions"
	dropEndpointBackoff dropReason = "endpoint_backoff"
	dropStoreError      dropReason = "store_error"
	dropSendFailed      dropReason = "send_failed"
)

// dropReasonOrder fixes the attribute order on the dispatch line. Ranging the
// outcome map directly would reorder attributes between identical events and
// make the log stream harder to diff.
var dropReasonOrder = []dropReason{
	dropMuted, dropDisabled, dropCooldown, dropNoSubs,
	dropEndpointBackoff, dropStoreError, dropSendFailed,
}

// dispatchOutcome accumulates the per-user results of one job's fan-out so the
// dispatcher can report what a notification actually did, rather than what it
// was about to attempt.
type dispatchOutcome struct {
	delivered int
	drops     map[dropReason]int
}

func (o *dispatchOutcome) record(reason dropReason) {
	if reason == dropDelivered {
		o.delivered++
		return
	}
	if o.drops == nil {
		o.drops = make(map[dropReason]int, 2)
	}
	o.drops[reason]++
}

// attrs renders the outcome as slog key/value pairs. Reasons that did not
// occur are omitted: a line of permanent zeroes is as unreadable as no line.
func (o *dispatchOutcome) attrs() []any {
	attrs := []any{"delivered", o.delivered}
	for _, reason := range dropReasonOrder {
		if n := o.drops[reason]; n > 0 {
			attrs = append(attrs, string(reason), n)
		}
	}
	return attrs
}

// Options bundles dispatcher construction params. Zero values fall back to
// documented defaults (see New).
type Options struct {
	Store          Store
	Sender         Sender
	VAPID          *VAPID
	SnapshotSigner *SnapshotSigner // nil → payloads omit image URLs
	Logger         *slog.Logger
	CooldownWindow time.Duration // default 3 min
	QueueCapacity  int           // default 256
	Workers        int           // default 4
	Metrics        *Metrics      // nil → NewMetrics()
}

// New builds a dispatcher. Call Start(ctx) to launch workers.
func New(opts Options) *NotificationDispatcher {
	if opts.CooldownWindow == 0 {
		opts.CooldownWindow = 3 * time.Minute
	}
	if opts.QueueCapacity == 0 {
		opts.QueueCapacity = 256
	}
	if opts.Workers == 0 {
		opts.Workers = 4
	}
	if opts.Metrics == nil {
		opts.Metrics = NewMetrics()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &NotificationDispatcher{
		store:          opts.Store,
		sender:         opts.Sender,
		vapid:          opts.VAPID,
		snapshotSigner: opts.SnapshotSigner,
		cooldown:       NewCooldownCache(opts.CooldownWindow, nil),
		backoff:        NewCooldownCache(60*time.Second, nil),
		metrics:        opts.Metrics,
		logger:         opts.Logger,
		window:         opts.CooldownWindow,
		jobs:           make(chan notificationJob, opts.QueueCapacity),
		workers:        opts.Workers,
	}
}

// VerifySnapshot validates a signed snapshot URL from the push
// notification handler. Returns nil on success; any error means the
// handler should 404. Push is disabled (nil signer) returns an error so
// the handler can still 404 cleanly.
func (d *NotificationDispatcher) VerifySnapshot(eventID, exp, sig string) error {
	if d.snapshotSigner == nil {
		return errors.New("snapshot signer not configured")
	}
	return d.snapshotSigner.Verify(eventID, exp, sig)
}

// Metrics returns the shared metrics struct. Used by the /metrics handler
// to render counters in Prometheus text format without reaching into the
// dispatcher's internals.
func (d *NotificationDispatcher) Metrics() *Metrics { return d.metrics }

// VAPIDPublicKey returns the base64url-encoded VAPID public key. The HTTP
// API exposes this to the browser so the service worker can subscribe.
func (d *NotificationDispatcher) VAPIDPublicKey() string {
	if d.vapid == nil {
		return ""
	}
	return d.vapid.PublicKey()
}

// Enqueue is non-blocking. If the queue is full the event is dropped and
// EventsDropped is incremented. The detection hot path must never be stalled
// on notification fan-out.
func (d *NotificationDispatcher) Enqueue(ev camera.Event) {
	d.enqueue(notificationJob{event: ev})
}

// EnqueueDoorbell queues a time-sensitive ring notification. It bypasses the
// per-camera notification cooldown, while still respecting mute, preferences,
// subscription state, and push-service backoff.
func (d *NotificationDispatcher) EnqueueDoorbell(ev camera.Event) bool {
	return d.enqueue(notificationJob{event: ev, immediate: true})
}

// EnqueueActivity is non-blocking and returns whether the incident was
// accepted. The processor only marks durable notification state after true.
func (d *NotificationDispatcher) EnqueueActivity(activity storage.Activity) bool {
	copy := activity
	return d.enqueue(notificationJob{event: activity.PrimaryEvent, activity: &copy})
}

func (d *NotificationDispatcher) enqueue(job notificationJob) bool {
	d.metrics.EventsReceived.Add(1)
	select {
	case d.jobs <- job:
		d.metrics.QueueDepth.Store(int64(len(d.jobs)))
		return true
	default:
		d.metrics.EventsDropped.Add(1)
		d.logger.Warn("notification queue full, dropping event",
			"event", job.event.ID, "camera", job.event.CameraName, "label", job.event.Label)
		return false
	}
}

// EnqueueTest synthesizes a delivery check for every subscription owned by the
// requesting operator. snapshotEventID may be empty; the service worker then
// falls back to Vedetta's notification icon.
func (d *NotificationDispatcher) EnqueueTest(username, cameraName, snapshotEventID string) {
	if cameraName == "" {
		cameraName = "test"
	}
	ev := camera.Event{
		ID:                fmt.Sprintf("test-%d", time.Now().UnixNano()),
		CameraName:        cameraName,
		Label:             "person",
		Timestamp:         time.Now().UTC(),
		SnapshotAvailable: false,
	}
	d.enqueue(notificationJob{
		event: ev, test: true, targetUser: username, snapshotEventID: snapshotEventID,
	})
}

// Start launches worker goroutines and a periodic cooldown sweeper.
// Returns immediately; workers run until ctx is cancelled.
func (d *NotificationDispatcher) Start(ctx context.Context) {
	for i := 0; i < d.workers; i++ {
		d.wg.Add(1)
		go d.workerLoop(ctx, i)
	}
	d.wg.Add(1)
	go d.sweepLoop(ctx)
}

// Wait blocks until all workers and the sweeper have exited. Callers should
// cancel the ctx passed to Start first.
func (d *NotificationDispatcher) Wait() { d.wg.Wait() }

func (d *NotificationDispatcher) sweepLoop(ctx context.Context) {
	defer d.wg.Done()
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.cooldown.Sweep()
			d.backoff.Sweep()
			if n, err := d.store.CountPushSubscriptions(); err == nil {
				d.metrics.SubscriptionCount.Store(int64(n))
			}
		}
	}
}

func (d *NotificationDispatcher) workerLoop(ctx context.Context, id int) {
	defer d.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-d.jobs:
			d.metrics.QueueDepth.Store(int64(len(d.jobs)))
			d.handleJob(ctx, job)
		}
	}
}

// handleEvent implements the per-event dispatch logic described in the
// design spec (each worker's inner loop, steps 1–7). A panic in any one
// event's handling must not kill the worker.
func (d *NotificationDispatcher) handleJob(ctx context.Context, job notificationJob) {
	ev := job.event
	defer func() {
		if r := recover(); r != nil {
			d.logger.Error("panic in notification worker",
				"event", ev.ID, "panic", r)
			d.metrics.PushSendError.Add(1)
		}
	}()

	users := []string{job.targetUser}
	if job.targetUser == "" {
		var err error
		users, err = d.store.ListAllUsernames()
		if err != nil {
			d.logger.Error("list users", "error", err)
			return
		}
	}
	payload := BuildPayload(ev, d.snapshotSigner)
	identity := ev.CameraName + ":" + ev.Label
	activityID := ""
	if job.activity != nil {
		payload = BuildActivityPayload(*job.activity, d.snapshotSigner)
		identity = "activity:" + job.activity.ID
		activityID = job.activity.ID
	} else if job.test {
		payload = BuildTestPayload(ev.CameraName, job.snapshotEventID, ev.Timestamp, d.snapshotSigner)
		identity = "test"
	}

	var outcome dispatchOutcome
	if job.test {
		outcome.record(d.dispatchTestToUser(ctx, job.targetUser, payload))
	} else {
		for _, user := range users {
			outcome.record(d.dispatchToUser(ctx, user, ev, identity, payload, job.immediate))
		}
	}

	// One outcome line per job, written after fan-out. Logging before the
	// gates made a dispatch that reached nobody read exactly like one that
	// reached every device. Payload characteristics ride along so production
	// pushes stay inspectable without a decrypted body in the log stream.
	attrs := []any{
		"event", ev.ID,
		"activity", activityID,
		"camera", ev.CameraName,
		"label", ev.Label,
		"snapshot_available", ev.SnapshotAvailable,
		"signer_configured", d.snapshotSigner != nil,
		"payload_bytes", len(payload),
		"has_image_field", bytesContainsImageField(payload),
		"users", len(users),
	}
	d.logger.Log(ctx, d.dispatchLevel(outcome), "push dispatch", append(attrs, outcome.attrs()...)...)
}

// dispatchLevel escalates a fan-out that delivered nothing while the process
// has never delivered anything at all. Ordinary suppression cannot reach that
// state: mute and preference gates are operator intent, and the cooldown gate
// is armed only by a prior successful send. Sustained zero delivery from
// startup instead means push is broken or switched off wholesale, which is the
// failure that otherwise stays invisible until someone asks why their phone
// has been quiet. A healthy install stops warning on its first delivery.
func (d *NotificationDispatcher) dispatchLevel(outcome dispatchOutcome) slog.Level {
	if outcome.delivered == 0 && d.metrics.EventsSent.Load() == 0 {
		return slog.LevelWarn
	}
	return slog.LevelInfo
}

// dispatchTestToUser deliberately bypasses alert mute, preference, cooldown,
// and endpoint backoff gates: an explicit delivery test must exercise every
// registered device. Transport accounting and stale-subscription pruning stay
// identical to production sends.
func (d *NotificationDispatcher) dispatchTestToUser(ctx context.Context, user string, payload []byte) dropReason {
	subs, err := d.store.ListPushSubscriptionsByUser(user)
	if err != nil {
		d.logger.Error("list test subscriptions", "user", user, "error", err)
		return dropStoreError
	}
	if len(subs) == 0 {
		d.metrics.EventsDisabled.Add(1)
		return dropNoSubs
	}
	anySuccess := false
	for _, sub := range subs {
		sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		result := d.sender.Send(sendCtx, Subscription{
			Endpoint: sub.Endpoint,
			P256dh:   sub.P256dh,
			Auth:     sub.Auth,
		}, payload, d.vapid)
		cancel()
		d.recordResult(result, sub.Endpoint)
		switch {
		case result.Status >= 200 && result.Status < 300:
			anySuccess = true
		case result.Status == 404 || result.Status == 410:
			if err := d.store.DeletePushSubscriptionByEndpoint(sub.Endpoint); err != nil {
				d.logger.Error("prune test subscription", "error", err)
			}
		case result.Status == 429:
			d.backoff.Mark(sub.Endpoint)
		}
	}
	if anySuccess {
		d.metrics.EventsSent.Add(1)
		return dropDelivered
	}
	return dropSendFailed
}

// bytesContainsImageField returns true if the JSON payload includes a
// non-empty "image" field. Cheap string scan — payloads are small JSON.
func bytesContainsImageField(p []byte) bool {
	s := string(p)
	i := indexOfImageKey(s)
	return i >= 0
}

func indexOfImageKey(s string) int {
	for i := 0; i+8 <= len(s); i++ {
		if s[i] == '"' && s[i+1] == 'i' && s[i+2] == 'm' && s[i+3] == 'a' &&
			s[i+4] == 'g' && s[i+5] == 'e' && s[i+6] == '"' && s[i+7] == ':' {
			return i
		}
	}
	return -1
}

// dispatchToUser applies every gate for one recipient and returns why the
// notification was not delivered, or dropDelivered if it was. The returned
// reason is finer-grained than the metric each gate increments: a user with no
// registered device and a user who switched notifications off share the
// EventsDisabled counter, and the /metrics contract keeps them merged, but the
// log distinguishes them.
func (d *NotificationDispatcher) dispatchToUser(ctx context.Context, user string, ev camera.Event, identity string, payload []byte, bypassCooldown bool) dropReason {
	// 1. Mute check.
	if muted, _, _ := d.store.GetKV("notify:" + user + ":muted"); muted == "1" {
		d.metrics.EventsMuted.Add(1)
		return dropMuted
	}
	// 2. Pref check (wildcard-aware via storage layer).
	enabled, err := d.store.IsNotificationEnabled(user, ev.CameraName, ev.Label)
	if err != nil {
		d.logger.Error("pref check", "user", user, "error", err)
		return dropStoreError
	}
	if !enabled {
		d.metrics.EventsDisabled.Add(1)
		return dropDisabled
	}
	// 3. Cooldown check.
	key := user + ":" + identity
	if !bypassCooldown && d.cooldown.Check(key) {
		d.metrics.EventsCooldown.Add(1)
		return dropCooldown
	}
	// 4. Load subscriptions.
	subs, err := d.store.ListPushSubscriptionsByUser(user)
	if err != nil {
		d.logger.Error("list subs", "user", user, "error", err)
		return dropStoreError
	}
	if len(subs) == 0 {
		d.metrics.EventsDisabled.Add(1)
		return dropNoSubs
	}

	// 5/6. Send to each subscription. Mark cooldown only if >=1 success —
	// a total failure (all 5xx or timeout) should not suppress the retry
	// opportunity on the next event.
	anySuccess := false
	attempted := false

	for _, sub := range subs {
		// Per-endpoint 60s backoff. 429s set this key so subsequent events
		// skip the offending endpoint until its backoff window expires.
		if d.backoff.Check(sub.Endpoint) {
			continue
		}
		attempted = true
		sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		result := d.sender.Send(sendCtx, Subscription{
			Endpoint: sub.Endpoint,
			P256dh:   sub.P256dh,
			Auth:     sub.Auth,
		}, payload, d.vapid)
		cancel()
		d.recordResult(result, sub.Endpoint)
		switch {
		case result.Status >= 200 && result.Status < 300:
			anySuccess = true
		case result.Status == 404 || result.Status == 410:
			if err := d.store.DeletePushSubscriptionByEndpoint(sub.Endpoint); err != nil {
				d.logger.Error("prune sub", "error", err)
			}
		case result.Status == 429:
			d.backoff.Mark(sub.Endpoint)
		}
	}
	if anySuccess {
		if !bypassCooldown {
			d.cooldown.Mark(key)
		}
		d.metrics.EventsSent.Add(1)
		return dropDelivered
	}
	if !attempted {
		return dropEndpointBackoff
	}
	return dropSendFailed
}

func (d *NotificationDispatcher) recordResult(r SendResult, endpoint string) {
	switch {
	case r.Timeout:
		d.metrics.PushSendTimeout.Add(1)
		d.logger.Warn("push send timeout", "endpoint_host", hostOnly(endpoint))
	case r.Err != nil:
		d.metrics.PushSendError.Add(1)
		d.logger.Warn("push send error",
			"endpoint_host", hostOnly(endpoint), "error", r.Err)
	case r.Status == 401 || r.Status == 403:
		d.metrics.PushSend401.Add(1)
		d.logger.Error("push send 401/403",
			"endpoint_host", hostOnly(endpoint), "status", r.Status, "body", r.Body)
	case r.Status == 410 || r.Status == 404:
		d.metrics.PushSend410.Add(1)
	case r.Status == 429:
		d.metrics.PushSend429.Add(1)
	case r.Status >= 200 && r.Status < 300:
		d.metrics.PushSendOK.Add(1)
	default:
		d.metrics.PushSendError.Add(1)
		d.logger.Warn("push send unexpected status",
			"endpoint_host", hostOnly(endpoint), "status", r.Status, "body", r.Body)
	}
}

// WebPushSender is the production Sender that calls webpush-go. The
// dispatcher owns its lifetime; tests substitute a fakeSender.
type WebPushSender struct {
	// Subscriber is the VAPID "sub" claim, typically "mailto:admin@example.com".
	// Set from config; if empty, uses "mailto:vedetta@localhost".
	Subscriber string
	// TTLSeconds overrides the default 60s if non-zero. Push services use
	// TTL to decide how long to hold an undelivered message.
	TTLSeconds int
}

// Send implements Sender by calling webpush.SendNotificationWithContext.
// Transport errors and context deadlines are surfaced via SendResult rather
// than exceptions, so the dispatcher can make pruning decisions without
// inspecting error types.
func (w *WebPushSender) Send(ctx context.Context, sub Subscription, payload []byte, vapid *VAPID) SendResult {
	if vapid == nil {
		return SendResult{Err: errors.New("webpush: vapid is nil")}
	}
	subscriber := w.Subscriber
	if subscriber == "" {
		// webpush-go prepends "mailto:" itself for non-https subscribers.
		// Pass a raw email; the library handles the scheme.
		subscriber = "vedetta@localhost"
	}
	ttl := w.TTLSeconds
	if ttl == 0 {
		ttl = 60
	}
	ws := &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys: webpush.Keys{
			P256dh: sub.P256dh,
			Auth:   sub.Auth,
		},
	}
	opts := &webpush.Options{
		VAPIDPublicKey:  vapid.PublicKey(),
		VAPIDPrivateKey: vapid.PrivateKey(),
		Subscriber:      subscriber,
		TTL:             ttl,
		Urgency:         webpush.UrgencyHigh,
	}
	resp, err := webpush.SendNotificationWithContext(ctx, payload, ws, opts)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return SendResult{Timeout: true, Err: err}
		}
		return SendResult{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	// Capture response body on non-2xx so the dispatcher can surface it
	// in logs — push services (especially Apple) return text bodies that
	// explain WHY a VAPID JWT was rejected, and hiding them turns
	// debugging into archaeology.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return SendResult{Status: resp.StatusCode, Body: string(body)}
	}
	return SendResult{Status: resp.StatusCode}
}

// hostOnly extracts the host portion of an endpoint URL without pulling in
// net/url. Logging full endpoints would leak unique subscription IDs; we
// keep just enough to identify which push service was involved.
func hostOnly(endpoint string) string {
	for i := 0; i < len(endpoint); i++ {
		if endpoint[i] == '/' && i+1 < len(endpoint) && endpoint[i+1] == '/' {
			rest := endpoint[i+2:]
			for j := 0; j < len(rest); j++ {
				if rest[j] == '/' {
					return rest[:j]
				}
			}
			return rest
		}
	}
	return endpoint
}
