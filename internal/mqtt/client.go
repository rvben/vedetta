package mqtt

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/netguard"
)

// guardedDialBroker is paho's connection opener. It dials the broker through
// the netguard dialer, whose Control hook rejects link-local / metadata
// addresses at connect time. Being a custom opener, it also avoids paho's
// proxy.FromEnvironment wrapping so the guard always inspects the real broker
// address rather than a SOCKS proxy socket.
func guardedDialBroker(uri *url.URL, o pahomqtt.ClientOptions) (net.Conn, error) {
	timeout := o.ConnectTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return netguard.Dialer(timeout).DialContext(context.Background(), "tcp", uri.Host)
}

// Publisher defines the interface for MQTT publishing operations.
type Publisher interface {
	PublishEvent(event camera.Event, matchedObjects []string) error
	PublishPresence(pe camera.PresenceEvent, objectName string) error
	PublishCameraStatus(cameraName string, online, stopped, sleeping bool)
	PublishDiscovery(cameraNames []string)
	PublishPresenceDiscovery(zones []ZoneInfo)
	PublishObjectCount(cameraName, label string, count int) error
	PublishDiskStatus(freeBytes, totalBytes uint64, recordingPaused bool)
	PublishDiskDiscovery()
	PublishDoorbellDiscovery(cameraNames []string)
	ClearDoorbellDiscovery(cameraNames []string)
	Close()
}

// defaultPublishTimeout bounds how long a publish waits for the broker to
// acknowledge. Several publishes run synchronously on the single event-loop
// goroutine, so an unbounded wait would let a wedged broker stall the entire
// NVR. Long enough for a healthy broker round-trip, short enough to fail fast.
const defaultPublishTimeout = 5 * time.Second

// Client wraps an MQTT connection for publishing detection events
// and Home Assistant MQTT discovery messages.
type Client struct {
	client         pahomqtt.Client
	topic          string
	publishTimeout time.Duration
}

// waitPublish bounds the wait for an in-flight publish so a wedged broker
// cannot block the caller (notably the event-loop goroutine) indefinitely. It
// returns the publish error, or a timeout error if the broker did not
// acknowledge within publishTimeout.
func (c *Client) waitPublish(token pahomqtt.Token) error {
	if !token.WaitTimeout(c.publishTimeout) {
		return fmt.Errorf("mqtt publish timed out after %s", c.publishTimeout)
	}
	return token.Error()
}

func New(cfg config.MQTTConfig) (*Client, error) {
	topic := cfg.Topic
	if topic == "" {
		topic = "vedetta"
	}

	availabilityTopic := topic + "/availability"

	opts := pahomqtt.NewClientOptions().
		AddBroker(fmt.Sprintf("tcp://%s:%d", cfg.Host, cfg.Port)).
		SetClientID("vedetta").
		SetAutoReconnect(true).
		SetWill(availabilityTopic, "offline", 1, true).
		// Open the broker connection ourselves so the SSRF policy is enforced
		// at connect time against the resolved broker address. A custom opener
		// also bypasses paho's proxy.FromEnvironment path, which would
		// otherwise apply the guard only to the SOCKS proxy socket and leave
		// the real broker target unchecked when ALL_PROXY is set.
		SetCustomOpenConnectionFn(guardedDialBroker)

	if cfg.Username != "" {
		opts.SetUsername(cfg.Username)
		opts.SetPassword(cfg.Password)
	}

	c := &Client{topic: topic, publishTimeout: defaultPublishTimeout}

	opts.SetOnConnectHandler(func(_ pahomqtt.Client) {
		slog.Info("MQTT connected, publishing availability")
		c.publishAvailability("online")
	})

	opts.SetConnectionLostHandler(func(_ pahomqtt.Client, err error) {
		slog.Warn("MQTT connection lost", "error", err)
	})

	client := pahomqtt.NewClient(opts)
	c.client = client
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		return nil, fmt.Errorf("mqtt connect: %w", token.Error())
	}

	slog.Info("connected to MQTT", "host", cfg.Host, "port", cfg.Port)

	return c, nil
}

func (c *Client) publishAvailability(status string) {
	topic := c.topic + "/availability"
	token := c.client.Publish(topic, 1, true, status)
	if err := c.waitPublish(token); err != nil {
		slog.Error("failed to publish availability", "error", err)
	}
}

func (c *Client) PublishEvent(event camera.Event, matchedObjects []string) error {
	type eventPayload struct {
		camera.Event
		Objects []string `json:"objects,omitempty"`
	}
	payload, err := json.Marshal(eventPayload{Event: event, Objects: matchedObjects})
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	topic := fmt.Sprintf("%s/events/%s", c.topic, event.CameraName)
	token := c.client.Publish(topic, 1, false, payload)
	return c.waitPublish(token)
}

func (c *Client) PublishObjectCount(cameraName, label string, count int) error {
	topic := fmt.Sprintf("%s/%s/%s", c.topic, cameraName, label)
	payload := strconv.Itoa(count)
	token := c.client.Publish(topic, 1, true, payload)
	if err := c.waitPublish(token); err != nil {
		slog.Error("failed to publish object count", "camera", cameraName, "label", label, "error", err)
		return err
	}
	return nil
}

func (c *Client) PublishSnapshot(cameraName, label string, jpegData []byte) {
	// Per-label snapshot (e.g., vedetta/front_door/person/snapshot)
	labelTopic := fmt.Sprintf("%s/%s/%s/snapshot", c.topic, cameraName, label)
	token := c.client.Publish(labelTopic, 0, true, jpegData)
	if err := c.waitPublish(token); err != nil {
		slog.Error("failed to publish label snapshot", "camera", cameraName, "label", label, "error", err)
	}

	// Latest snapshot for this camera (e.g., vedetta/front_door/snapshot)
	cameraTopic := fmt.Sprintf("%s/%s/snapshot", c.topic, cameraName)
	token = c.client.Publish(cameraTopic, 0, true, jpegData)
	if err := c.waitPublish(token); err != nil {
		slog.Error("failed to publish camera snapshot", "camera", cameraName, "error", err)
	}
}

func (c *Client) PublishDoorbell(cameraName, person string, jpegData []byte) {
	payload, _ := json.Marshal(map[string]string{
		"camera": cameraName,
		"person": person,
		"type":   "doorbell",
	})
	topic := fmt.Sprintf("%s/%s/doorbell", c.topic, cameraName)
	token := c.client.Publish(topic, 1, false, payload)
	if err := c.waitPublish(token); err != nil {
		slog.Error("failed to publish doorbell", "camera", cameraName, "error", err)
	}

	// Also publish snapshot on the doorbell snapshot topic (retained)
	if len(jpegData) > 0 {
		snapTopic := fmt.Sprintf("%s/%s/doorbell/snapshot", c.topic, cameraName)
		token = c.client.Publish(snapTopic, 0, true, jpegData)
		if err := c.waitPublish(token); err != nil {
			slog.Error("failed to publish doorbell snapshot", "camera", cameraName, "error", err)
		}
	}
}

func (c *Client) PublishPresence(pe camera.PresenceEvent, objectName string) error {
	// Map explicitly. Defaulting anything that is not a leave to "entered" turned
	// a zone that had merely lost sight of an object into a claim that the object
	// was there.
	var state string
	switch pe.Type {
	case camera.EventZoneEnter:
		state = "entered"
	case camera.EventZoneLeave:
		state = "left"
	case camera.EventZoneUnknown:
		state = "unknown"
	default:
		slog.Warn("unhandled presence event type, not publishing",
			"type", pe.Type, "zone", pe.ZoneName, "label", pe.Label)
		return nil
	}
	m := map[string]string{
		"zone":  pe.ZoneName,
		"label": pe.Label,
		"state": state,
	}
	if objectName != "" {
		m["object"] = objectName
	}
	payload, err := json.Marshal(m)
	if err != nil {
		return err
	}

	topic := fmt.Sprintf("%s/presence/%s/%s", c.topic, sanitizeName(pe.ZoneName), pe.Label)
	token := c.client.Publish(topic, 1, true, payload)
	if err := c.waitPublish(token); err != nil {
		slog.Error("failed to publish presence", "zone", pe.ZoneName, "error", err)
		return err
	}
	return nil
}

func (c *Client) PublishObjectSighting(objectName string, event camera.Event) {
	payload, err := json.Marshal(map[string]any{
		"object":   objectName,
		"camera":   event.CameraName,
		"label":    event.Label,
		"event_id": event.ID,
		"zone":     event.ZoneName,
	})
	if err != nil {
		return
	}

	topic := fmt.Sprintf("%s/objects/%s/sighted", c.topic, sanitizeName(objectName))
	token := c.client.Publish(topic, 1, true, payload)
	if err := c.waitPublish(token); err != nil {
		slog.Error("failed to publish object sighting", "object", objectName, "error", err)
	}
}

// PublishCameraStatus publishes the camera's raw state. Home Assistant's
// discovery value template maps the non-fault sleeping and stopped states to a
// connected binary sensor while preserving the richer state for MQTT clients.
func (c *Client) PublishCameraStatus(cameraName string, online, stopped, sleeping bool) {
	status := "OFF"
	switch {
	case stopped:
		status = "stopped"
	case online:
		status = "ON"
	case sleeping:
		status = "sleeping"
	}
	topic := fmt.Sprintf("%s/camera/%s/status", c.topic, cameraName)
	token := c.client.Publish(topic, 1, true, status)
	if err := c.waitPublish(token); err != nil {
		slog.Error("failed to publish camera status",
			"camera", cameraName, "error", err)
	}
}

// PublishDiscovery publishes Home Assistant MQTT discovery messages for each camera.
func (c *Client) PublishDiscovery(cameraNames []string) {
	for _, name := range cameraNames {
		c.publishCameraDiscovery(name)
	}
}

// PublishObjectDiscovery publishes HA discovery for tracked objects as device triggers.
func (c *Client) PublishObjectDiscovery(objects []ObjectInfo) {
	for _, obj := range objects {
		c.publishObjectTriggerDiscovery(obj)
	}
}

// ObjectInfo carries the minimal info needed for MQTT discovery.
type ObjectInfo struct {
	Name  string
	Label string
}

// ZoneInfo carries the minimal info needed for zone presence MQTT discovery.
type ZoneInfo struct {
	ZoneName string
	Label    string
}

// PublishPresenceDiscovery publishes HA binary_sensor discovery for each zone+label combination.
func (c *Client) PublishPresenceDiscovery(zones []ZoneInfo) {
	for _, z := range zones {
		c.publishPresenceSensorDiscovery(z)
	}
}

func (c *Client) publishPresenceSensorDiscovery(z ZoneInfo) {
	zoneSafe := sanitizeName(z.ZoneName)
	labelSafe := sanitizeName(z.Label)
	objectID := fmt.Sprintf("vedetta_%s_%s", zoneSafe, labelSafe)

	device := haDevice{
		Identifiers:  []string{"vedetta_zone_" + zoneSafe},
		Name:         "Vedetta " + z.ZoneName,
		Manufacturer: "Vedetta",
		Model:        "Zone Presence",
	}

	stateTopic := fmt.Sprintf("%s/presence/%s/%s", c.topic, zoneSafe, labelSafe)
	sensorConfig := haPresenceSensorConfig{
		Name:     fmt.Sprintf("%s %s", z.ZoneName, z.Label),
		UniqueID: objectID,
		Availability: []haAvailability{
			{Topic: c.topic + "/availability"},
			{
				Topic:         stateTopic,
				ValueTemplate: "{{ 'offline' if value_json.state == 'unknown' else 'online' }}",
			},
		},
		AvailabilityMode: "all",
		StateTopic:       stateTopic,
		DeviceClass:      "occupancy",
		ValueTemplate:    "{{ value_json.state }}",
		PayloadOn:        "entered",
		PayloadOff:       "left",
		Device:           device,
	}

	payload, err := json.Marshal(sensorConfig)
	if err != nil {
		return
	}

	topic := fmt.Sprintf("homeassistant/binary_sensor/%s/config", objectID)
	token := c.client.Publish(topic, 1, true, payload)
	if err := c.waitPublish(token); err != nil {
		slog.Error("failed to publish presence discovery", "zone", z.ZoneName, "label", z.Label, "error", err)
	}
}

func (c *Client) publishObjectTriggerDiscovery(obj ObjectInfo) {
	objectID := fmt.Sprintf("vedetta_object_%s", sanitizeName(obj.Name))

	device := haDevice{
		Identifiers:  []string{objectID},
		Name:         "Vedetta " + obj.Name,
		Manufacturer: "Vedetta",
		Model:        "Tracked Object",
	}

	triggerConfig := haDeviceTriggerConfig{
		AutomationType: "trigger",
		Type:           "object_sighted",
		Subtype:        sanitizeName(obj.Label),
		Topic:          fmt.Sprintf("%s/objects/%s/sighted", c.topic, sanitizeName(obj.Name)),
		Device:         device,
	}

	payload, err := json.Marshal(triggerConfig)
	if err != nil {
		return
	}

	topic := fmt.Sprintf("homeassistant/device_automation/%s_sighted/config", objectID)
	token := c.client.Publish(topic, 1, true, payload)
	if err := c.waitPublish(token); err != nil {
		slog.Error("failed to publish object trigger discovery", "object", obj.Name, "error", err)
	}
}

func (c *Client) publishCameraDiscovery(cameraName string) {
	objectID := fmt.Sprintf("vedetta_%s", sanitizeName(cameraName))

	device := haDevice{
		Identifiers:  []string{"vedetta_" + sanitizeName(cameraName)},
		Name:         "Vedetta " + cameraName,
		Manufacturer: "Vedetta",
		Model:        "NVR",
	}

	// Binary sensor for camera online/offline status
	sensorConfig := haBinarySensorConfig{
		Name:              cameraName,
		UniqueID:          objectID + "_status",
		StateTopic:        fmt.Sprintf("%s/camera/%s/status", c.topic, cameraName),
		AvailabilityTopic: c.topic + "/availability",
		DeviceClass:       "connectivity",
		PayloadOn:         "ON",
		PayloadOff:        "OFF",
		// A sleeping battery camera and a camera deliberately stopped by its
		// operator are not connectivity failures. Without this normalization,
		// Home Assistant leaves the entity unknown because neither raw value
		// matches payload_on/payload_off, which is surfaced as unavailable and
		// can trigger an outage alert.
		ValueTemplate: "{{ 'OFF' if value == 'OFF' else 'ON' }}",
		Device:        device,
	}

	sensorPayload, err := json.Marshal(sensorConfig)
	if err != nil {
		slog.Error("failed to marshal discovery config", "camera", cameraName, "error", err)
		return
	}

	sensorTopic := fmt.Sprintf("homeassistant/binary_sensor/%s/config", objectID)
	token := c.client.Publish(sensorTopic, 1, true, sensorPayload)
	if err := c.waitPublish(token); err != nil {
		slog.Error("failed to publish discovery", "camera", cameraName, "error", err)
	}

	// Device trigger for detection events
	triggerConfig := haDeviceTriggerConfig{
		AutomationType: "trigger",
		Type:           "detection",
		Subtype:        "object_detected",
		Topic:          fmt.Sprintf("%s/events/%s", c.topic, cameraName),
		Device:         device,
	}

	triggerPayload, err := json.Marshal(triggerConfig)
	if err != nil {
		slog.Error("failed to marshal trigger config", "camera", cameraName, "error", err)
		return
	}

	triggerTopic := fmt.Sprintf("homeassistant/device_automation/%s_detection/config", objectID)
	token = c.client.Publish(triggerTopic, 1, true, triggerPayload)
	if err := c.waitPublish(token); err != nil {
		slog.Error("failed to publish trigger discovery", "camera", cameraName, "error", err)
	}

	// MQTT image entity for detection snapshots
	imageConfig := haImageConfig{
		Name:              cameraName + " Last Detection",
		UniqueID:          objectID + "_snapshot",
		ImageTopic:        fmt.Sprintf("%s/%s/snapshot", c.topic, cameraName),
		AvailabilityTopic: c.topic + "/availability",
		Device:            device,
	}

	imagePayload, err := json.Marshal(imageConfig)
	if err == nil {
		imageTopic := fmt.Sprintf("homeassistant/image/%s_snapshot/config", objectID)
		token = c.client.Publish(imageTopic, 1, true, imagePayload)
		if err := c.waitPublish(token); err != nil {
			slog.Error("failed to publish image discovery", "camera", cameraName, "error", err)
		}
	}

	slog.Info("published HA discovery", "camera", cameraName)
}

// buildDoorbellDiscovery returns the HA MQTT discovery topic and JSON payload for
// a camera's doorbell as a device-automation trigger. The Device identifier
// matches publishCameraDiscovery so the doorbell trigger groups under the
// existing camera device in Home Assistant rather than creating a duplicate.
func buildDoorbellDiscovery(topicPrefix, cameraName string) (string, []byte) {
	sanitized := sanitizeName(cameraName)
	cfg := haDeviceTriggerConfig{
		AutomationType: "trigger",
		Type:           "button_short_press",
		Subtype:        "press",
		Topic:          fmt.Sprintf("%s/%s/doorbell", topicPrefix, cameraName),
		Device: haDevice{
			Identifiers:  []string{"vedetta_" + sanitized},
			Name:         "Vedetta " + cameraName,
			Manufacturer: "Vedetta",
			Model:        "NVR",
		},
	}
	payload, _ := json.Marshal(cfg)
	topic := fmt.Sprintf("homeassistant/device_automation/vedetta_%s_doorbell/config", sanitized)
	return topic, payload
}

// PublishDoorbellDiscovery publishes retained HA MQTT discovery for each doorbell-enabled camera.
func (c *Client) PublishDoorbellDiscovery(cameraNames []string) {
	for _, name := range cameraNames {
		topic, payload := buildDoorbellDiscovery(c.topic, name)
		token := c.client.Publish(topic, 1, true, payload)
		if err := c.waitPublish(token); err != nil {
			slog.Error("failed to publish doorbell discovery", "camera", name, "error", err)
		}
	}
}

// ClearDoorbellDiscovery removes the retained doorbell discovery for each camera by
// publishing an empty retained payload, causing Home Assistant to drop the entity.
func (c *Client) ClearDoorbellDiscovery(cameraNames []string) {
	for _, name := range cameraNames {
		topic, _ := buildDoorbellDiscovery(c.topic, name)
		token := c.client.Publish(topic, 1, true, []byte{})
		if err := c.waitPublish(token); err != nil {
			slog.Error("failed to clear doorbell discovery", "camera", name, "error", err)
		}
	}
}

// BuildDiskStatusPayload constructs the JSON body published to vedetta/status/disk.
// Extracted for testability — PublishDiskStatus calls this.
func BuildDiskStatusPayload(freeBytes, totalBytes uint64, recordingPaused bool) []byte {
	usedPct := uint64(0)
	if totalBytes > 0 {
		usedPct = 100 - (freeBytes*100)/totalBytes
	}
	payload := map[string]any{
		"free_bytes":       freeBytes,
		"total_bytes":      totalBytes,
		"free_mb":          freeBytes / (1024 * 1024),
		"total_mb":         totalBytes / (1024 * 1024),
		"used_percent":     usedPct,
		"recording_paused": recordingPaused,
		"timestamp":        time.Now().UTC().Format(time.RFC3339),
	}
	body, _ := json.Marshal(payload)
	return body
}

// PublishDiskStatus publishes free/total byte counts and the recording-paused
// flag as a retained JSON payload to vedetta/status/disk.
func (c *Client) PublishDiskStatus(freeBytes, totalBytes uint64, recordingPaused bool) {
	body := BuildDiskStatusPayload(freeBytes, totalBytes, recordingPaused)
	topic := c.topic + "/status/disk"
	token := c.client.Publish(topic, 1, true, body)
	if err := c.waitPublish(token); err != nil {
		slog.Error("failed to publish disk status", "topic", topic, "error", err)
	}
}

// PublishDiskDiscovery publishes Home Assistant MQTT discovery configs for the
// disk-free sensor, disk-used percentage sensor, and recording-paused binary sensor.
func (c *Client) PublishDiskDiscovery() {
	base := c.topic + "/status/disk"
	availTopic := c.topic + "/availability"

	freeCfg := map[string]any{
		"name":                "Vedetta Disk Free",
		"unique_id":           "vedetta_disk_free_mb",
		"state_topic":         base,
		"value_template":      "{{ value_json.free_mb }}",
		"unit_of_measurement": "MB",
		"device_class":        "data_size",
		"availability_topic":  availTopic,
	}
	usedCfg := map[string]any{
		"name":                "Vedetta Disk Used",
		"unique_id":           "vedetta_disk_used_pct",
		"state_topic":         base,
		"value_template":      "{{ value_json.used_percent }}",
		"unit_of_measurement": "%",
		"availability_topic":  availTopic,
	}
	pausedCfg := map[string]any{
		"name":               "Vedetta Recording Paused",
		"unique_id":          "vedetta_recording_paused",
		"state_topic":        base,
		"value_template":     "{{ 'ON' if value_json.recording_paused else 'OFF' }}",
		"device_class":       "problem",
		"payload_on":         "ON",
		"payload_off":        "OFF",
		"availability_topic": availTopic,
	}

	discoveries := map[string]map[string]any{
		"homeassistant/sensor/vedetta_disk_free/config":     freeCfg,
		"homeassistant/sensor/vedetta_disk_used/config":     usedCfg,
		"homeassistant/binary_sensor/vedetta_paused/config": pausedCfg,
	}
	for discoveryTopic, cfg := range discoveries {
		body, err := json.Marshal(cfg)
		if err != nil {
			slog.Error("failed to marshal disk discovery", "topic", discoveryTopic, "error", err)
			continue
		}
		token := c.client.Publish(discoveryTopic, 1, true, body)
		if err := c.waitPublish(token); err != nil {
			slog.Error("failed to publish disk discovery", "topic", discoveryTopic, "error", err)
		}
	}
}

func (c *Client) Close() {
	c.publishAvailability("offline")
	c.client.Disconnect(1000)
}

// sanitizeName converts a camera name to a safe identifier for MQTT topics.
func sanitizeName(name string) string {
	r := strings.NewReplacer(" ", "_", "-", "_", ".", "_")
	return strings.ToLower(r.Replace(name))
}

// Home Assistant discovery payload types

type haDevice struct {
	Identifiers  []string `json:"identifiers"`
	Name         string   `json:"name"`
	Manufacturer string   `json:"manufacturer"`
	Model        string   `json:"model"`
}

type haBinarySensorConfig struct {
	Name              string   `json:"name"`
	UniqueID          string   `json:"unique_id"`
	StateTopic        string   `json:"state_topic"`
	AvailabilityTopic string   `json:"availability_topic"`
	DeviceClass       string   `json:"device_class"`
	PayloadOn         string   `json:"payload_on"`
	PayloadOff        string   `json:"payload_off"`
	ValueTemplate     string   `json:"value_template,omitempty"`
	Device            haDevice `json:"device"`
}

// haAvailability is one entry of Home Assistant's `availability` list.
type haAvailability struct {
	Topic         string `json:"topic"`
	ValueTemplate string `json:"value_template,omitempty"`
}

type haPresenceSensorConfig struct {
	Name     string `json:"name"`
	UniqueID string `json:"unique_id"`
	// Availability carries two entries, combined with AvailabilityMode "all":
	// the broker-level LWT, and the presence state itself. A zone reporting
	// "unknown" has lost sight of the object, which is neither on nor off, so it
	// renders as unavailable instead of silently reading as "no car here".
	Availability     []haAvailability `json:"availability"`
	AvailabilityMode string           `json:"availability_mode"`
	StateTopic       string           `json:"state_topic"`
	DeviceClass      string           `json:"device_class"`
	ValueTemplate    string           `json:"value_template"`
	PayloadOn        string           `json:"payload_on"`
	PayloadOff       string           `json:"payload_off"`
	Device           haDevice         `json:"device"`
}

type haImageConfig struct {
	Name              string   `json:"name"`
	UniqueID          string   `json:"unique_id"`
	ImageTopic        string   `json:"image_topic"`
	AvailabilityTopic string   `json:"availability_topic"`
	Device            haDevice `json:"device"`
}

type haDeviceTriggerConfig struct {
	AutomationType string   `json:"automation_type"`
	Type           string   `json:"type"`
	Subtype        string   `json:"subtype"`
	Topic          string   `json:"topic"`
	Device         haDevice `json:"device"`
}
