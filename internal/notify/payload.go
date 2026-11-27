package notify

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/cry"
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
		p.URL = doorbellAnswerURL(ev.CameraName, ev.ID)
	}
	if ev.Kind == camera.EventKindCry {
		p.Title = "Crying heard"
		p.Body = friendlyCameraName(ev.CameraName) + " · Open to listen"
		p.Tag = ev.CameraName + ":cry"
		p.URL = "/camera.html?name=" + url.QueryEscape(ev.CameraName)
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
		// Replace the immediate ring notification instead of stacking a second
		// card once the Activity quiet period completes.
		p.Tag = activity.CameraName + ":doorbell"
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

// BuildCryStatusPayload produces the notice that crying recognition stopped
// working for a camera, or works again. Both share one tag, so on a device
// that replaces by tag the recovery takes the place of the outage card.
func BuildCryStatusPayload(n cry.OutageNotice) []byte {
	p := pushPayload{
		Title: "Crying alerts are not working",
		Body:  friendlyCameraName(n.Camera) + " · " + n.Reason,
		URL:   "/camera.html?name=" + url.QueryEscape(n.Camera),
		Tag:   n.Camera + ":cry-status",
		TS:    n.At.UTC().Unix(),
	}
	if n.Recovered {
		p.Title = "Crying alerts are working again"
		p.Body = friendlyCameraName(n.Camera) + " · They were out for " + roughDuration(n.At.Sub(n.Since))
	}
	data, _ := json.Marshal(p)
	return data
}

// cryStatusEvent is the event a crying recognition notice travels as. It
// carries the cry label so the mute and per-camera preference that govern
// crying alerts govern this notice too, and no snapshot.
func cryStatusEvent(n cry.OutageNotice) camera.Event {
	return camera.Event{
		ID:         fmt.Sprintf("cry-status-%s-%d", n.Camera, n.At.Unix()),
		CameraName: n.Camera,
		Label:      "baby_cry",
		Timestamp:  n.At,
	}
}

// roughDuration renders a span the way a person would say it.
func roughDuration(d time.Duration) string {
	minutes := int(d.Round(time.Minute) / time.Minute)
	switch {
	case minutes < 1:
		return "less than a minute"
	case minutes == 1:
		return "1 minute"
	case minutes < 60:
		return fmt.Sprintf("%d minutes", minutes)
	}
	hours, rest := minutes/60, minutes%60
	text := fmt.Sprintf("%d hours", hours)
	if hours == 1 {
		text = "1 hour"
	}
	if rest > 0 {
		text += fmt.Sprintf(" %d min", rest)
	}
	return text
}

func doorbellAnswerURL(cameraName, eventID string) string {
	return "/doorbell-answer.html?camera=" + url.QueryEscape(cameraName) + "&event=" + url.QueryEscape(eventID)
}

// BuildTestPayload produces the operator-requested delivery check. When a
// retained camera snapshot exists, it uses the same signed image route as real
// detection and Activity notifications; otherwise the service worker's icon
// remains the deliberate fallback.
func BuildTestPayload(cameraName, snapshotEventID string, at time.Time, signer *SnapshotSigner) []byte {
	p := pushPayload{
		Title: "Vedetta notification test",
		Body:  "Notifications are working · " + friendlyCameraName(cameraName),
		URL:   "/settings.html",
		Tag:   "vedetta-test",
		TS:    at.UTC().Unix(),
	}
	if snapshotEventID != "" && signer != nil {
		p.Image = signer.Sign(snapshotEventID)
	}
	data, _ := json.Marshal(p)
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
