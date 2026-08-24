package notify

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/storage"
)

// pushPayload is the JSON shape delivered to the service worker.
type pushPayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
	Image string `json:"image,omitempty"` // omitted when SnapshotAvailable is false
	Tag   string `json:"tag"`
	TS    int64  `json:"ts"`
}

// BuildPayload produces the JSON push body for a detection event.
// See design spec → "Service worker" and "payload.go" sections.
//
// When signer is non-nil and ev.SnapshotAvailable is true, the payload's
// image field is set to a short-lived HMAC-signed URL that iOS can fetch
// anonymously (no session cookies) to render the notification thumbnail.
// The authenticated /api/events/<id>/snapshot endpoint returns 401 to
// unauthenticated fetches, which iOS silently treats as "no image".
func BuildPayload(ev camera.Event, signer *SnapshotSigner) []byte {
	p := pushPayload{
		Title: friendlyCameraName(ev.CameraName),
		Body:  fmt.Sprintf("%s detected · %s UTC", titleCase(ev.Label), ev.Timestamp.UTC().Format("15:04")),
		URL:   fmt.Sprintf("/event.html?id=%s", ev.ID),
		Tag:   fmt.Sprintf("%s:%s", ev.CameraName, ev.Label),
		TS:    ev.Timestamp.UTC().Unix(),
	}
	if ev.Kind == camera.EventKindDoorbell || ev.Label == "doorbell" {
		p.Title = "Someone's at the door"
		p.Body = friendlyCameraName(ev.CameraName) + " · " + ev.Timestamp.UTC().Format("15:04") + " UTC"
		if ev.SubLabel != "" {
			p.Title = ev.SubLabel + " is at the door"
		}
		p.Tag = ev.CameraName + ":doorbell"
	}
	if ev.SnapshotAvailable && signer != nil {
		p.Image = signer.Sign(ev.ID)
	}
	data, _ := json.Marshal(p)
	if len(data) > 4000 {
		// Defensive truncation: drop image first (already conditional above),
		// then clip body. Extreme case only.
		p.Image = ""
		if len(p.Body) > 120 {
			p.Body = p.Body[:120]
		}
		data, _ = json.Marshal(p)
	}
	return data
}

// BuildActivityPayload produces one notification for a finalized incident.
func BuildActivityPayload(activity storage.Activity, signer *SnapshotSigner) []byte {
	ev := activity.PrimaryEvent
	labels := make([]string, 0, len(activity.Labels))
	for _, label := range activity.Labels {
		labels = append(labels, titleCase(label))
	}
	labelSummary := strings.Join(labels, ", ")
	if labelSummary == "" {
		labelSummary = "Activity"
	}
	evidence := "event"
	if activity.EventCount != 1 {
		evidence = "events"
	}
	p := pushPayload{
		Title: friendlyCameraName(activity.CameraName),
		Body: fmt.Sprintf("%s · %d %s · %s UTC", labelSummary, activity.EventCount, evidence,
			activity.StartTime.UTC().Format("15:04")),
		URL: "/activity.html?id=" + url.QueryEscape(activity.ID),
		Tag: "activity:" + activity.ID,
		TS:  activity.StartTime.UTC().Unix(),
	}
	if activity.HasDoorbell {
		p.Title = "Someone's at the door"
		p.Body = friendlyCameraName(activity.CameraName) + " · " + activity.StartTime.UTC().Format("15:04") + " UTC"
		if len(activity.RecognizedNames) > 0 {
			p.Title = activity.RecognizedNames[0] + " is at the door"
		}
	}
	if ev.SnapshotAvailable && signer != nil {
		p.Image = signer.Sign(ev.ID)
	}
	data, _ := json.Marshal(p)
	if len(data) > 4000 {
		p.Image = ""
		if len(p.Body) > 120 {
			p.Body = p.Body[:120]
		}
		data, _ = json.Marshal(p)
	}
	return data
}

// friendlyCameraName turns a config-level camera identifier like
// "kids_bedroom_3" or "front_door" into a display string suitable for a
// notification title: "Kids Bedroom 3", "Front Door". Numeric suffixes
// stay numeric. This is a cosmetic fallback — a future DisplayName
// field in CameraConfig should take priority once it exists.
func friendlyCameraName(name string) string {
	if name == "" {
		return name
	}
	parts := strings.FieldsFunc(name, func(r rune) bool {
		return r == '_' || r == '-'
	})
	for i, part := range parts {
		parts[i] = titleCase(part)
	}
	return strings.Join(parts, " ")
}

// titleCase uppercases the first byte of an ASCII word. Good enough for
// English detection labels like "person", "car", "bicycle".
func titleCase(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[0] >= 'a' && b[0] <= 'z' {
		b[0] -= 'a' - 'A'
	}
	return string(b)
}
