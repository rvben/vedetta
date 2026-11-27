package mqtt

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

// cryRecognitionProblemTemplate turns the raw recognition state into Home
// Assistant's problem sensor: on for the states in which a cry would go
// unheard, off for every other, so a camera that is starting, listening,
// crying or stopped by its operator reads as fine.
const cryRecognitionProblemTemplate = "{{ 'ON' if value in ['unavailable', 'unsupported', 'overloaded'] else 'OFF' }}"

func cryRecognitionStateTopic(topicPrefix, cameraName string) string {
	return fmt.Sprintf("%s/camera/%s/cry_recognition", topicPrefix, cameraName)
}

// buildCryRecognitionDiscovery returns the HA MQTT discovery topic and JSON
// payload for a camera's crying recognition as a problem binary sensor. The
// device identifier matches publishCameraDiscovery, so the sensor sits on the
// camera's existing device.
func buildCryRecognitionDiscovery(topicPrefix, cameraName string) (string, []byte) {
	sanitized := sanitizeName(cameraName)
	cfg := haBinarySensorConfig{
		Name:              "Crying recognition",
		UniqueID:          "vedetta_" + sanitized + "_cry_recognition",
		StateTopic:        cryRecognitionStateTopic(topicPrefix, cameraName),
		AvailabilityTopic: topicPrefix + "/availability",
		DeviceClass:       "problem",
		PayloadOn:         "ON",
		PayloadOff:        "OFF",
		ValueTemplate:     cryRecognitionProblemTemplate,
		Device: haDevice{
			Identifiers:  []string{"vedetta_" + sanitized},
			Name:         "Vedetta " + cameraName,
			Manufacturer: "Vedetta",
			Model:        "NVR",
		},
	}
	payload, _ := json.Marshal(cfg)
	topic := fmt.Sprintf("homeassistant/binary_sensor/vedetta_%s_cry_recognition/config", sanitized)
	return topic, payload
}

// PublishCryRecognitionDiscovery publishes retained HA MQTT discovery for each
// camera that runs crying recognition.
func (c *Client) PublishCryRecognitionDiscovery(cameraNames []string) {
	for _, name := range cameraNames {
		topic, payload := buildCryRecognitionDiscovery(c.topic, name)
		token := c.client.Publish(topic, 1, true, payload)
		if err := c.waitPublish(token); err != nil {
			slog.Error("failed to publish crying recognition discovery", "camera", name, "error", err)
		}
	}
}

// ClearCryRecognitionDiscovery removes the retained discovery and the retained
// state for each camera by publishing empty retained payloads, so Home
// Assistant drops the entity when recognition is turned off.
func (c *Client) ClearCryRecognitionDiscovery(cameraNames []string) {
	for _, name := range cameraNames {
		discovery, _ := buildCryRecognitionDiscovery(c.topic, name)
		for _, topic := range []string{discovery, cryRecognitionStateTopic(c.topic, name)} {
			token := c.client.Publish(topic, 1, true, []byte{})
			if err := c.waitPublish(token); err != nil {
				slog.Error("failed to clear crying recognition discovery", "camera", name, "error", err)
			}
		}
	}
}

// PublishCryRecognition publishes a camera's raw recognition state, such as
// "listening" or "unavailable".
func (c *Client) PublishCryRecognition(cameraName, state string) {
	token := c.client.Publish(cryRecognitionStateTopic(c.topic, cameraName), 1, true, state)
	if err := c.waitPublish(token); err != nil {
		slog.Error("failed to publish crying recognition state", "camera", cameraName, "error", err)
	}
}
