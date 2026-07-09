package camera

import (
	"encoding/json"
	"time"

	"github.com/rvben/vedetta/internal/detect"
)

// matchZones tags each tracked object with the zones it overlaps and derives the
// presence keys for zones with TrackPresence enabled.
//
// trackZones covers every tracked object, because event emission needs the zones
// of a coasting track to close its event correctly.
//
// zoneMatches covers only objects a detection matched on this frame. Presence
// must report what the detector saw, never what the tracker still believes: a
// stationary track coasts for 50x the normal disappearance budget, so a parked
// car's track outlives the car. Feeding coasting tracks to the presence state
// machine kept driveway/car "entered" for 4.5 hours after the car drove off,
// with no detections in that window. The tracker's own leave debounce is what
// absorbs the gaps between detections; it can only do that if the gaps are
// visible to it.
func matchZones(zones []Zone, tracked []detect.TrackedObject, frameW, frameH int) (map[PresenceKey]bool, map[int][]Zone) {
	zoneMatches := make(map[PresenceKey]bool)
	trackZones := make(map[int][]Zone, len(tracked))

	for _, obj := range tracked {
		matched := MatchZones(zones, obj.Box, obj.Label, frameW, frameH)
		trackZones[obj.TrackID] = matched
		if !obj.Detected {
			continue
		}
		for _, z := range matched {
			if z.TrackPresence {
				zoneMatches[PresenceKey{ZoneID: z.ID, Label: obj.Label}] = true
			}
		}
	}

	return zoneMatches, trackZones
}

// Zone represents a spatial region on a camera view.
// Coordinates are percentages (0.0-1.0) relative to the frame dimensions.
type Zone struct {
	ID              int         `json:"id"`
	Camera          string      `json:"camera"`
	Name            string      `json:"name"`
	Points          [][]float64 `json:"points"`
	X1              float64     `json:"-"`
	Y1              float64     `json:"-"`
	X2              float64     `json:"-"`
	Y2              float64     `json:"-"`
	Labels          []string    `json:"labels"`
	TrackPresence   bool        `json:"track_presence"`
	FaceRecognition bool        `json:"face_recognition"`
	Enabled         bool        `json:"enabled"`
}

// ZonePresence tracks the presence state of a label within a zone.
type ZonePresence struct {
	ZoneID      int       `json:"zone_id"`
	Label       string    `json:"label"`
	Present     bool      `json:"present"`
	LastSeen    time.Time `json:"last_seen,omitempty"`
	LastChanged time.Time `json:"last_changed,omitempty"`
}

// LabelsJSON returns the JSON representation of the zone's labels.
func (z Zone) LabelsJSON() string {
	data, _ := json.Marshal(z.Labels)
	return string(data)
}

// MatchZones returns the zones that contain the detection anchor point.
// The anchor point is the bottom-center of the detection box, normalized to 0..1.
func MatchZones(zones []Zone, box [4]int, label string, frameW, frameH int) []Zone {
	if frameW <= 0 || frameH <= 0 {
		return nil
	}

	if box[2] <= box[0] || box[3] <= box[1] {
		return nil
	}
	ax := float64(box[0]+box[2]) / 2 / float64(frameW)
	ay := float64(box[3]) / float64(frameH)

	var matched []Zone
	for _, z := range zones {
		if !z.Enabled {
			continue
		}

		if !zoneMatchesLabel(z, label) {
			continue
		}
		if len(z.Points) < 3 {
			continue
		}
		if pointInPolygon(ax, ay, z.Points) {
			matched = append(matched, z)
		}
	}

	return matched
}

func zoneMatchesLabel(z Zone, label string) bool {
	if len(z.Labels) == 0 {
		return true
	}
	for _, l := range z.Labels {
		if l == label {
			return true
		}
	}
	return false
}

func pointInPolygon(x, y float64, polygon [][]float64) bool {
	inside := false
	j := len(polygon) - 1
	for i := range polygon {
		if len(polygon[i]) != 2 || len(polygon[j]) != 2 {
			j = i
			continue
		}
		xi, yi := polygon[i][0], polygon[i][1]
		xj, yj := polygon[j][0], polygon[j][1]
		intersects := ((yi > y) != (yj > y)) &&
			(x < (xj-xi)*(y-yi)/(yj-yi+1e-12)+xi)
		if intersects {
			inside = !inside
		}
		j = i
	}
	return inside
}
