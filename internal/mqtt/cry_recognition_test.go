package mqtt

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildCryRecognitionDiscovery(t *testing.T) {
	topic, payload := buildCryRecognitionDiscovery("vedetta", "Kids Bedroom")
	if topic != "homeassistant/binary_sensor/vedetta_kids_bedroom_cry_recognition/config" {
		t.Errorf("topic = %q", topic)
	}
	var cfg map[string]any
	if err := json.Unmarshal(payload, &cfg); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"name":               "Crying recognition",
		"unique_id":          "vedetta_kids_bedroom_cry_recognition",
		"state_topic":        "vedetta/camera/Kids Bedroom/cry_recognition",
		"availability_topic": "vedetta/availability",
		"device_class":       "problem",
		"payload_on":         "ON",
		"payload_off":        "OFF",
	} {
		if cfg[key] != want {
			t.Errorf("%s = %v, want %q", key, cfg[key], want)
		}
	}

	// The sensor belongs to the camera's existing device, not a second one.
	device, _ := cfg["device"].(map[string]any)
	ids, _ := device["identifiers"].([]any)
	if len(ids) != 1 || ids[0] != "vedetta_kids_bedroom" {
		t.Errorf("device identifiers = %v, want [vedetta_kids_bedroom]", device["identifiers"])
	}
}

// The template is the only place that decides which states are a problem, and
// Home Assistant evaluates it, not this process. Pin the set, so a state added
// to it or dropped from it is a visible change.
func TestCryRecognitionTemplateNamesExactlyTheUnheardStates(t *testing.T) {
	start := strings.Index(cryRecognitionProblemTemplate, "[")
	end := strings.Index(cryRecognitionProblemTemplate, "]")
	if start < 0 || end < start {
		t.Fatalf("template has no state list: %s", cryRecognitionProblemTemplate)
	}
	var got []string
	for _, quoted := range strings.Split(cryRecognitionProblemTemplate[start+1:end], ",") {
		got = append(got, strings.Trim(strings.TrimSpace(quoted), "'"))
	}
	want := []string{"unavailable", "unsupported", "overloaded"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("problem states = %v, want %v", got, want)
	}
	if !strings.HasPrefix(cryRecognitionProblemTemplate, "{{ 'ON' if value in [") ||
		!strings.HasSuffix(cryRecognitionProblemTemplate, "] else 'OFF' }}") {
		t.Errorf("template does not map the listed states to ON and the rest to OFF: %s", cryRecognitionProblemTemplate)
	}
}

func TestClientPublishCryRecognition_RawStateRetained(t *testing.T) {
	c, f := newTestClient()
	c.PublishCryRecognition("nursery", "unavailable")
	got := requireOnePublish(t, f)
	if got.topic != "vedetta/camera/nursery/cry_recognition" {
		t.Errorf("topic = %q", got.topic)
	}
	if string(got.payload) != "unavailable" {
		t.Errorf("payload = %q, want the raw state", got.payload)
	}
	if got.qos != 1 || !got.retained {
		t.Errorf("qos = %d retained = %v, want QoS 1 retained so a late subscriber sees the state", got.qos, got.retained)
	}
}

func TestClientPublishCryRecognitionDiscovery_Retained(t *testing.T) {
	c, f := newTestClient()
	c.PublishCryRecognitionDiscovery([]string{"nursery"})
	got := requireOnePublish(t, f)
	if got.topic != "homeassistant/binary_sensor/vedetta_nursery_cry_recognition/config" {
		t.Errorf("topic = %q", got.topic)
	}
	if got.qos != 1 || !got.retained || len(got.payload) == 0 {
		t.Errorf("qos = %d retained = %v payload = %d bytes", got.qos, got.retained, len(got.payload))
	}
}

func TestClientClearCryRecognitionDiscovery_EmptiesDiscoveryAndState(t *testing.T) {
	c, f := newTestClient()
	c.ClearCryRecognitionDiscovery([]string{"nursery"})
	calls := f.calls()
	if len(calls) != 2 {
		t.Fatalf("published %d messages, want the discovery and the state cleared", len(calls))
	}
	want := []string{
		"homeassistant/binary_sensor/vedetta_nursery_cry_recognition/config",
		"vedetta/camera/nursery/cry_recognition",
	}
	for i, call := range calls {
		if call.topic != want[i] {
			t.Errorf("call %d topic = %q, want %q", i, call.topic, want[i])
		}
		if len(call.payload) != 0 || !call.retained {
			t.Errorf("call %d: payload %q retained %v, want an empty retained message", i, call.payload, call.retained)
		}
	}
}
