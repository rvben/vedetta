package mqtt

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/rvben/vedetta/internal/camera"
)

// recordedPublish captures one Publish call so tests can assert on the exact
// topic, QoS, retain flag, and payload that reach the broker.
type recordedPublish struct {
	topic    string
	qos      byte
	retained bool
	payload  []byte
}

// fakeToken implements pahomqtt.Token with a fixed error result. When stall is
// set, WaitTimeout reports the deadline elapsed (the broker never acked),
// modelling a wedged broker.
type fakeToken struct {
	err   error
	stall bool
}

func (t *fakeToken) Wait() bool                     { return true }
func (t *fakeToken) WaitTimeout(time.Duration) bool { return !t.stall }
func (t *fakeToken) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
func (t *fakeToken) Error() error { return t.err }

// fakePaho implements pahomqtt.Client, recording every Publish call and
// returning a token carrying publishErr.
type fakePaho struct {
	mu         sync.Mutex
	publishes  []recordedPublish
	publishErr error
	stall      bool // when set, returned tokens never ack within the timeout
}

func (f *fakePaho) Publish(topic string, qos byte, retained bool, payload any) pahomqtt.Token {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b []byte
	switch p := payload.(type) {
	case []byte:
		b = append([]byte(nil), p...)
	case string:
		b = []byte(p)
	}
	f.publishes = append(f.publishes, recordedPublish{topic: topic, qos: qos, retained: retained, payload: b})
	return &fakeToken{err: f.publishErr, stall: f.stall}
}

func (f *fakePaho) calls() []recordedPublish {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedPublish, len(f.publishes))
	copy(out, f.publishes)
	return out
}

func (f *fakePaho) IsConnected() bool       { return true }
func (f *fakePaho) IsConnectionOpen() bool  { return true }
func (f *fakePaho) Connect() pahomqtt.Token { return &fakeToken{} }
func (f *fakePaho) Disconnect(uint)         {}
func (f *fakePaho) Subscribe(string, byte, pahomqtt.MessageHandler) pahomqtt.Token {
	return &fakeToken{}
}
func (f *fakePaho) SubscribeMultiple(map[string]byte, pahomqtt.MessageHandler) pahomqtt.Token {
	return &fakeToken{}
}
func (f *fakePaho) Unsubscribe(...string) pahomqtt.Token     { return &fakeToken{} }
func (f *fakePaho) AddRoute(string, pahomqtt.MessageHandler) {}
func (f *fakePaho) OptionsReader() pahomqtt.ClientOptionsReader {
	return pahomqtt.ClientOptionsReader{}
}

func newTestClient() (*Client, *fakePaho) {
	f := &fakePaho{}
	return &Client{client: f, topic: "vedetta", publishTimeout: defaultPublishTimeout}, f
}

func requireOnePublish(t *testing.T, f *fakePaho) recordedPublish {
	t.Helper()
	calls := f.calls()
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 publish, got %d", len(calls))
	}
	return calls[0]
}

func TestClientPublishEvent_TopicQoSAndPayload(t *testing.T) {
	c, f := newTestClient()
	event := camera.Event{ID: "e1", CameraName: "front", Label: "person", Timestamp: time.Unix(0, 0).UTC()}

	if err := c.PublishEvent(event, []string{"alice"}); err != nil {
		t.Fatalf("PublishEvent: %v", err)
	}

	got := requireOnePublish(t, f)
	if got.topic != "vedetta/events/front" {
		t.Errorf("topic = %q, want vedetta/events/front", got.topic)
	}
	if got.qos != 1 {
		t.Errorf("qos = %d, want 1", got.qos)
	}
	if got.retained {
		t.Error("event must not be retained")
	}
	var decoded map[string]any
	if err := json.Unmarshal(got.payload, &decoded); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if decoded["id"] != "e1" || decoded["camera"] != "front" {
		t.Errorf("payload missing event fields: %v", decoded)
	}
	objs, ok := decoded["objects"].([]any)
	if !ok || len(objs) != 1 || objs[0] != "alice" {
		t.Errorf("objects = %v, want [alice]", decoded["objects"])
	}
}

func TestClientPublishEvent_PropagatesTokenError(t *testing.T) {
	c, f := newTestClient()
	f.publishErr = errors.New("broker down")

	err := c.PublishEvent(camera.Event{ID: "e1", CameraName: "front"}, nil)
	if !errors.Is(err, f.publishErr) {
		t.Fatalf("PublishEvent error = %v, want broker down", err)
	}
}

// A publish whose broker never acknowledges must return a bounded timeout
// error rather than blocking forever. This matters because several publishes
// run synchronously on the single event-loop goroutine; an unbounded wait there
// stalls the entire NVR if the broker wedges.
func TestClientPublishEvent_TimesOutWhenBrokerStalls(t *testing.T) {
	c, f := newTestClient()
	f.stall = true
	c.publishTimeout = 5 * time.Millisecond

	err := c.PublishEvent(camera.Event{ID: "e1", CameraName: "front"}, nil)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("PublishEvent on a stalled broker = %v, want a publish timeout error", err)
	}
}

func TestClientPublishObjectCount_RetainedCount(t *testing.T) {
	c, f := newTestClient()
	c.PublishObjectCount("front", "person", 3)

	got := requireOnePublish(t, f)
	if got.topic != "vedetta/front/person" {
		t.Errorf("topic = %q, want vedetta/front/person", got.topic)
	}
	if !got.retained {
		t.Error("object count must be retained")
	}
	if string(got.payload) != "3" {
		t.Errorf("payload = %q, want 3", got.payload)
	}
}

// PublishObjectCount and PublishPresence run on the event loop wrapped in a
// publish span; they must return the publish error (in addition to logging it)
// so a wedged-broker timeout is recorded on the span rather than swallowed.
func TestClientPublishObjectCount_PropagatesError(t *testing.T) {
	c, f := newTestClient()
	f.publishErr = errors.New("broker down")
	if err := c.PublishObjectCount("front", "person", 1); !errors.Is(err, f.publishErr) {
		t.Fatalf("PublishObjectCount error = %v, want broker down", err)
	}
}

func TestClientPublishPresence_PropagatesError(t *testing.T) {
	c, f := newTestClient()
	f.publishErr = errors.New("broker down")
	err := c.PublishPresence(camera.PresenceEvent{Type: "zone_enter", ZoneName: "z", Label: "person"}, "")
	if !errors.Is(err, f.publishErr) {
		t.Fatalf("PublishPresence error = %v, want broker down", err)
	}
}

func TestClientPublishCameraStatus_States(t *testing.T) {
	cases := []struct {
		online, stopped bool
		want            string
	}{
		{true, false, "ON"},
		{false, false, "OFF"},
		{true, true, "stopped"},
		{false, true, "stopped"},
	}
	for _, tc := range cases {
		c, f := newTestClient()
		c.PublishCameraStatus("front", tc.online, tc.stopped)
		got := requireOnePublish(t, f)
		if got.topic != "vedetta/camera/front/status" {
			t.Errorf("topic = %q", got.topic)
		}
		if !got.retained {
			t.Error("camera status must be retained")
		}
		if string(got.payload) != tc.want {
			t.Errorf("online=%v stopped=%v: payload = %q, want %q", tc.online, tc.stopped, got.payload, tc.want)
		}
	}
}

func TestClientPublishPresence_EnterLeaveAndObject(t *testing.T) {
	c, f := newTestClient()
	c.PublishPresence(camera.PresenceEvent{Type: "zone_enter", ZoneName: "Front Yard", Label: "person"}, "alice")

	got := requireOnePublish(t, f)
	if got.topic != "vedetta/presence/front_yard/person" {
		t.Errorf("topic = %q, want vedetta/presence/front_yard/person", got.topic)
	}
	if !got.retained {
		t.Error("presence must be retained")
	}
	var m map[string]string
	if err := json.Unmarshal(got.payload, &m); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if m["state"] != "entered" {
		t.Errorf("state = %q, want entered", m["state"])
	}
	if m["object"] != "alice" {
		t.Errorf("object = %q, want alice", m["object"])
	}

	// zone_leave maps to state=left, and an empty object name is omitted.
	c2, f2 := newTestClient()
	c2.PublishPresence(camera.PresenceEvent{Type: "zone_leave", ZoneName: "Front Yard", Label: "person"}, "")
	got2 := requireOnePublish(t, f2)
	var m2 map[string]string
	if err := json.Unmarshal(got2.payload, &m2); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if m2["state"] != "left" {
		t.Errorf("state = %q, want left", m2["state"])
	}
	if _, ok := m2["object"]; ok {
		t.Errorf("object should be omitted when empty, got %q", m2["object"])
	}
}

func TestClientPublishObjectSighting_Topic(t *testing.T) {
	c, f := newTestClient()
	c.PublishObjectSighting("Alice Smith", camera.Event{ID: "e1", CameraName: "front", Label: "person", ZoneName: "porch"})

	got := requireOnePublish(t, f)
	if got.topic != "vedetta/objects/alice_smith/sighted" {
		t.Errorf("topic = %q, want vedetta/objects/alice_smith/sighted", got.topic)
	}
	var m map[string]any
	if err := json.Unmarshal(got.payload, &m); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if m["object"] != "Alice Smith" || m["event_id"] != "e1" || m["zone"] != "porch" {
		t.Errorf("payload missing fields: %v", m)
	}
}

func TestClientPublishSnapshot_PublishesLabelAndCameraTopics(t *testing.T) {
	c, f := newTestClient()
	c.PublishSnapshot("front", "person", []byte{0xff, 0xd8, 0xff})

	calls := f.calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 publishes (label + camera), got %d", len(calls))
	}
	topics := map[string]recordedPublish{calls[0].topic: calls[0], calls[1].topic: calls[1]}
	label, ok := topics["vedetta/front/person/snapshot"]
	if !ok {
		t.Fatalf("missing label snapshot topic; got %v", calls)
	}
	if label.qos != 0 || !label.retained {
		t.Errorf("label snapshot qos=%d retained=%v, want 0/true", label.qos, label.retained)
	}
	if _, ok := topics["vedetta/front/snapshot"]; !ok {
		t.Fatalf("missing camera snapshot topic; got %v", calls)
	}
}

func TestClientPublishDoorbell_EventAndSnapshot(t *testing.T) {
	c, f := newTestClient()
	c.PublishDoorbell("front", "alice", []byte{0xff, 0xd8})

	calls := f.calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 publishes (doorbell + snapshot), got %d", len(calls))
	}
	if calls[0].topic != "vedetta/front/doorbell" {
		t.Errorf("doorbell topic = %q", calls[0].topic)
	}
	var m map[string]string
	if err := json.Unmarshal(calls[0].payload, &m); err != nil {
		t.Fatalf("doorbell payload not JSON: %v", err)
	}
	if m["person"] != "alice" || m["type"] != "doorbell" {
		t.Errorf("doorbell payload = %v", m)
	}
	if calls[1].topic != "vedetta/front/doorbell/snapshot" {
		t.Errorf("snapshot topic = %q", calls[1].topic)
	}
}

func TestClientPublishDoorbell_NoSnapshotWhenEmpty(t *testing.T) {
	c, f := newTestClient()
	c.PublishDoorbell("front", "alice", nil)
	if calls := f.calls(); len(calls) != 1 {
		t.Fatalf("expected only the doorbell event publish, got %d", len(calls))
	}
}

func TestClientPublishDiskStatus_TopicAndPayload(t *testing.T) {
	c, f := newTestClient()
	c.PublishDiskStatus(1*1024*1024*1024, 10*1024*1024*1024, true)

	got := requireOnePublish(t, f)
	if got.topic != "vedetta/status/disk" {
		t.Errorf("topic = %q, want vedetta/status/disk", got.topic)
	}
	if !got.retained {
		t.Error("disk status must be retained")
	}
	var m map[string]any
	if err := json.Unmarshal(got.payload, &m); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if m["recording_paused"] != true {
		t.Errorf("recording_paused = %v, want true", m["recording_paused"])
	}
}

// A zone that lost sight of an object must not publish "entered" or "left". The
// old mapping defaulted every non-leave event to "entered", so a zone_unknown
// would have re-asserted presence for a car nobody could see.
func TestClientPublishPresence_UnknownIsNotEnteredOrLeft(t *testing.T) {
	c, f := newTestClient()
	c.PublishPresence(camera.PresenceEvent{Type: "zone_unknown", ZoneName: "Driveway", Label: "car"}, "Renault Trafic")

	got := requireOnePublish(t, f)
	if got.topic != "vedetta/presence/driveway/car" {
		t.Errorf("topic = %q, want vedetta/presence/driveway/car", got.topic)
	}
	var m map[string]string
	if err := json.Unmarshal(got.payload, &m); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if m["state"] != "unknown" {
		t.Errorf("zone_unknown published state = %q, want unknown", m["state"])
	}
}

// Home Assistant must render an unknown zone as unavailable rather than "off".
// The discovery config therefore has to carry an availability entry that maps the
// state payload, alongside the broker-level availability topic.
func TestClientPresenceDiscovery_UnknownMapsToUnavailable(t *testing.T) {
	c, f := newTestClient()
	c.publishPresenceSensorDiscovery(ZoneInfo{ZoneName: "Driveway", Label: "car"})

	got := requireOnePublish(t, f)
	var cfg map[string]any
	if err := json.Unmarshal(got.payload, &cfg); err != nil {
		t.Fatalf("discovery payload not JSON: %v", err)
	}
	avail, ok := cfg["availability"].([]any)
	if !ok || len(avail) < 2 {
		t.Fatalf("discovery must declare both the broker and the state availability entries, got %v", cfg["availability"])
	}
	if cfg["availability_mode"] != "all" {
		t.Errorf("availability_mode = %v, want all", cfg["availability_mode"])
	}
	var sawStateTemplate bool
	for _, a := range avail {
		entry, _ := a.(map[string]any)
		if tmpl, _ := entry["value_template"].(string); tmpl != "" && entry["topic"] == cfg["state_topic"] {
			sawStateTemplate = true
			if !json.Valid([]byte(`"` + tmpl + `"`)) {
				t.Errorf("availability template is not a JSON-safe string: %q", tmpl)
			}
		}
	}
	if !sawStateTemplate {
		t.Errorf("no availability entry maps the presence state topic to online/offline")
	}
}
