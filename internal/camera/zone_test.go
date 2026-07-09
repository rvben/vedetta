package camera

import (
	"testing"

	"github.com/rvben/vedetta/internal/detect"
)

func makeTestZone(name string, x1, y1, x2, y2 float64, labels []string) Zone {
	return Zone{
		ID:     1,
		Camera: "test",
		Name:   name,
		Points: [][]float64{
			{x1, y1},
			{x2, y1},
			{x2, y2},
			{x1, y2},
		},
		X1:      x1,
		Y1:      y1,
		X2:      x2,
		Y2:      y2,
		Labels:  labels,
		Enabled: true,
	}
}

func TestMatchZones_BasicOverlap(t *testing.T) {
	// Zone covers left half of frame (0.0-0.5 horizontal)
	zones := []Zone{makeTestZone("left", 0.0, 0.0, 0.5, 1.0, nil)}

	// Detection fully inside the zone (pixels 10-90 out of 200 wide = 5%-45%)
	matched := MatchZones(zones, [4]int{10, 10, 90, 90}, "person", 200, 200)
	if len(matched) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matched))
	}
	if matched[0].Name != "left" {
		t.Errorf("expected zone 'left', got %q", matched[0].Name)
	}
}

func TestMatchZones_NoOverlap(t *testing.T) {
	// Zone covers left half
	zones := []Zone{makeTestZone("left", 0.0, 0.0, 0.5, 1.0, nil)}

	// Detection fully in the right half (pixels 120-190 out of 200 = 60%-95%)
	matched := MatchZones(zones, [4]int{120, 10, 190, 90}, "person", 200, 200)
	if len(matched) != 0 {
		t.Fatalf("expected 0 matches, got %d", len(matched))
	}
}

func TestMatchZones_PartialOverlap_Below50Percent(t *testing.T) {
	// Zone covers left half (0.0-0.5)
	zones := []Zone{makeTestZone("left", 0.0, 0.0, 0.5, 1.0, nil)}

	// Detection mostly in the right half: x from 80-180 out of 200 = 40%-90%
	// Overlap with zone: 40%-50% = 10% of frame width
	// Detection width: 50% of frame
	// Overlap area / detection area = 10%/50% * height = 0.2 (20%) < 50%
	matched := MatchZones(zones, [4]int{80, 0, 180, 200}, "person", 200, 200)
	if len(matched) != 0 {
		t.Fatalf("expected 0 matches for <50%% overlap, got %d", len(matched))
	}
}

func TestMatchZones_PartialOverlap_Above50Percent(t *testing.T) {
	// Zone covers left 60% (0.0-0.6)
	zones := []Zone{makeTestZone("left", 0.0, 0.0, 0.6, 1.0, nil)}

	// Detection: x from 20-100 out of 200 = 10%-50%
	// Fully inside zone (zone goes to 60%), overlap = 100% > 50%
	matched := MatchZones(zones, [4]int{20, 0, 100, 199}, "person", 200, 200)
	if len(matched) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matched))
	}
}

func TestMatchZones_LabelFiltering(t *testing.T) {
	zones := []Zone{
		makeTestZone("driveway", 0.0, 0.0, 1.0, 1.0, []string{"car", "truck"}),
	}

	// Person should not match
	matched := MatchZones(zones, [4]int{10, 10, 90, 90}, "person", 100, 100)
	if len(matched) != 0 {
		t.Fatalf("expected 0 matches for wrong label, got %d", len(matched))
	}

	// Car should match
	matched = MatchZones(zones, [4]int{10, 10, 90, 90}, "car", 100, 100)
	if len(matched) != 1 {
		t.Fatalf("expected 1 match for car, got %d", len(matched))
	}

	// Truck should match
	matched = MatchZones(zones, [4]int{10, 10, 90, 90}, "truck", 100, 100)
	if len(matched) != 1 {
		t.Fatalf("expected 1 match for truck, got %d", len(matched))
	}
}

func TestMatchZones_EmptyLabels_MatchesAll(t *testing.T) {
	zones := []Zone{
		makeTestZone("all", 0.0, 0.0, 1.0, 1.0, nil),
	}

	matched := MatchZones(zones, [4]int{10, 10, 90, 90}, "person", 100, 100)
	if len(matched) != 1 {
		t.Fatalf("expected 1 match with empty labels, got %d", len(matched))
	}

	matched = MatchZones(zones, [4]int{10, 10, 90, 90}, "car", 100, 100)
	if len(matched) != 1 {
		t.Fatalf("expected 1 match for any label, got %d", len(matched))
	}
}

func TestMatchZones_DisabledZone(t *testing.T) {
	z := makeTestZone("disabled", 0.0, 0.0, 1.0, 1.0, nil)
	z.Enabled = false
	zones := []Zone{z}

	matched := MatchZones(zones, [4]int{10, 10, 90, 90}, "person", 100, 100)
	if len(matched) != 0 {
		t.Fatalf("expected 0 matches for disabled zone, got %d", len(matched))
	}
}

func TestMatchZones_MultipleZones(t *testing.T) {
	zones := []Zone{
		makeTestZone("left", 0.0, 0.0, 0.5, 1.0, nil),
		makeTestZone("right", 0.5, 0.0, 1.0, 1.0, nil),
		makeTestZone("full", 0.0, 0.0, 1.0, 1.0, nil),
	}

	// Detection in left half: matches "left" and "full"
	matched := MatchZones(zones, [4]int{5, 5, 45, 95}, "person", 100, 100)
	if len(matched) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(matched))
	}

	names := map[string]bool{}
	for _, m := range matched {
		names[m.Name] = true
	}
	if !names["left"] || !names["full"] {
		t.Errorf("expected left and full, got %v", names)
	}
}

func TestMatchZones_ZeroFrameDimensions(t *testing.T) {
	zones := []Zone{makeTestZone("z", 0.0, 0.0, 1.0, 1.0, nil)}

	matched := MatchZones(zones, [4]int{10, 10, 90, 90}, "person", 0, 0)
	if len(matched) != 0 {
		t.Fatalf("expected 0 matches with zero frame, got %d", len(matched))
	}
}

func TestMatchZones_ZeroAreaDetection(t *testing.T) {
	zones := []Zone{makeTestZone("z", 0.0, 0.0, 1.0, 1.0, nil)}

	// Point detection (zero area)
	matched := MatchZones(zones, [4]int{50, 50, 50, 50}, "person", 100, 100)
	if len(matched) != 0 {
		t.Fatalf("expected 0 matches for zero-area detection, got %d", len(matched))
	}
}

func TestMatchZones_EmptyZoneList(t *testing.T) {
	matched := MatchZones(nil, [4]int{10, 10, 90, 90}, "person", 100, 100)
	if len(matched) != 0 {
		t.Fatalf("expected 0 matches for empty zone list, got %d", len(matched))
	}
}

func TestMatchZones_DetectionAtEdge(t *testing.T) {
	zones := []Zone{makeTestZone("z", 0.0, 0.0, 0.5, 0.5, nil)}

	// Detection exactly at the boundary: 0.4-0.6 horizontal and vertical
	// Overlap: 0.4-0.5 x 0.4-0.5 = 0.1 x 0.1 = 0.01
	// Detection area: 0.2 x 0.2 = 0.04
	// Overlap/detection = 0.01/0.04 = 0.25 (25%) < 50%
	matched := MatchZones(zones, [4]int{40, 40, 60, 60}, "person", 100, 100)
	if len(matched) != 0 {
		t.Fatalf("expected 0 matches for 25%% overlap, got %d", len(matched))
	}
}

// A stationary track coasts for 50x the normal disappearance budget, so a parked
// car's track outlives the car itself. Presence must follow what the detector
// saw this frame, not what the tracker still believes, or a zone reports a car
// that drove away hours ago. Observed in production: driveway/car stayed
// "entered" for 4.5 hours after the car left, with zero detections in between.
func TestMatchZones_CoastingTrackDoesNotSustainPresence(t *testing.T) {
	zone := makeTestZone("driveway", 0, 0, 1, 1, []string{"car"})
	zone.TrackPresence = true
	zones := []Zone{zone}
	box := [4]int{10, 10, 90, 90}
	key := PresenceKey{ZoneID: zone.ID, Label: "car"}

	seen := []detect.TrackedObject{{TrackID: 1, Label: "car", Box: box, Detected: true}}
	inZone, detected, trackZones := matchZones(zones, seen, 100, 100)
	if !inZone[key][1] {
		t.Errorf("car detected in zone this frame: presence match = false, want true")
	}
	if !detected[1] {
		t.Errorf("car detected this frame: detected = false, want true")
	}
	if len(trackZones[1]) != 1 {
		t.Errorf("detected track should still be tagged with its zone, got %d zones", len(trackZones[1]))
	}

	coasting := []detect.TrackedObject{{TrackID: 1, Label: "car", Box: box, Detected: false}}
	inZone, detected, trackZones = matchZones(zones, coasting, 100, 100)
	if inZone[key][1] {
		t.Errorf("track coasting with no detection: presence match = true, want false")
	}
	if detected[1] {
		t.Errorf("track coasting with no detection: detected = true, want false")
	}
	// Event emission still needs the zone tag for a coasting track.
	if len(trackZones[1]) != 1 {
		t.Errorf("coasting track must still be tagged with its zone, got %d zones", len(trackZones[1]))
	}
}
