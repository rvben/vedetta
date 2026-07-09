package camera

import (
	"testing"
	"time"
)

func newTestTracker() *PresenceTracker {
	pt := NewPresenceTracker()
	pt.debounceEnter = 3 * time.Second
	pt.debounceLeave = 30 * time.Second
	return pt
}

var testZoneNames = map[int]string{1: "driveway", 2: "doorbell"}

func TestPresence_EnterDebounce(t *testing.T) {
	pt := newTestTracker()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	pt.now = func() time.Time { return now }

	key := PresenceKey{ZoneID: 1, Label: "car"}

	// First detection: start enter timer, no event yet
	events := pt.Update(map[PresenceKey]bool{key: true}, testZoneNames)
	if len(events) != 0 {
		t.Fatalf("expected 0 events on first detection, got %d", len(events))
	}

	// 2 seconds later: still below debounce threshold
	now = now.Add(2 * time.Second)
	events = pt.Update(map[PresenceKey]bool{key: true}, testZoneNames)
	if len(events) != 0 {
		t.Fatalf("expected 0 events before debounce, got %d", len(events))
	}

	// 1 more second (total 3s): debounce met, should enter
	now = now.Add(1 * time.Second)
	events = pt.Update(map[PresenceKey]bool{key: true}, testZoneNames)
	if len(events) != 1 {
		t.Fatalf("expected 1 enter event, got %d", len(events))
	}
	if events[0].Type != "zone_enter" {
		t.Errorf("expected zone_enter, got %q", events[0].Type)
	}
	if events[0].Label != "car" {
		t.Errorf("expected label 'car', got %q", events[0].Label)
	}
	if events[0].ZoneName != "driveway" {
		t.Errorf("expected zone 'driveway', got %q", events[0].ZoneName)
	}
}

// The legacy Update cannot see tracks, so it cannot tell a departure from a loss.
// It must therefore report the honest answer, Unknown, and never claim a leave it
// did not observe.
func TestPresence_LegacyUpdateReportsUnknownNotLeaveOnDisappearance(t *testing.T) {
	pt := newTestTracker()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	pt.now = func() time.Time { return now }

	key := PresenceKey{ZoneID: 1, Label: "car"}

	// Enter the zone (skip debounce by advancing past it)
	pt.Update(map[PresenceKey]bool{key: true}, testZoneNames)
	now = now.Add(4 * time.Second)
	pt.Update(map[PresenceKey]bool{key: true}, testZoneNames)

	// Now stop detecting
	now = now.Add(1 * time.Second)
	events := pt.Update(map[PresenceKey]bool{}, testZoneNames)
	if len(events) != 0 {
		t.Fatalf("expected 0 events on first miss, got %d", len(events))
	}

	// 29 seconds later: still below the lost timeout
	now = now.Add(29 * time.Second)
	events = pt.Update(map[PresenceKey]bool{}, testZoneNames)
	if len(events) != 0 {
		t.Fatalf("expected 0 events before the lost timeout, got %d", len(events))
	}

	// 1 more second (total 30s): the zone admits it does not know
	now = now.Add(1 * time.Second)
	events = pt.Update(map[PresenceKey]bool{}, testZoneNames)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != EventZoneUnknown {
		t.Errorf("object disappeared without being seen to leave: got %q, want %q",
			events[0].Type, EventZoneUnknown)
	}
	if status, _ := pt.GetState(key); status != PresenceUnknown {
		t.Errorf("object disappeared without being seen to leave: state = %v, want unknown", status)
	}
}

func TestPresence_CancelEnterOnGap(t *testing.T) {
	pt := newTestTracker()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	pt.now = func() time.Time { return now }

	key := PresenceKey{ZoneID: 1, Label: "car"}

	// Start detecting
	pt.Update(map[PresenceKey]bool{key: true}, testZoneNames)

	// 2 seconds later
	now = now.Add(2 * time.Second)
	pt.Update(map[PresenceKey]bool{key: true}, testZoneNames)

	// Gap: stop detecting for a frame
	now = now.Add(500 * time.Millisecond)
	pt.Update(map[PresenceKey]bool{}, testZoneNames)

	// Resume detecting: enter timer should restart
	now = now.Add(500 * time.Millisecond)
	pt.Update(map[PresenceKey]bool{key: true}, testZoneNames)

	// 2 seconds later: should NOT enter yet (timer was reset)
	now = now.Add(2 * time.Second)
	events := pt.Update(map[PresenceKey]bool{key: true}, testZoneNames)
	if len(events) != 0 {
		t.Fatalf("expected 0 events (timer reset after gap), got %d", len(events))
	}

	// 1 more second (3s total since re-detection): should enter now
	now = now.Add(1 * time.Second)
	events = pt.Update(map[PresenceKey]bool{key: true}, testZoneNames)
	if len(events) != 1 {
		t.Fatalf("expected 1 enter event, got %d", len(events))
	}
}

func TestPresence_CancelLeaveOnDetection(t *testing.T) {
	pt := newTestTracker()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	pt.now = func() time.Time { return now }

	key := PresenceKey{ZoneID: 1, Label: "car"}

	// Enter zone
	pt.Update(map[PresenceKey]bool{key: true}, testZoneNames)
	now = now.Add(4 * time.Second)
	pt.Update(map[PresenceKey]bool{key: true}, testZoneNames)

	// Stop detecting for 20 seconds (below leave threshold)
	now = now.Add(20 * time.Second)
	pt.Update(map[PresenceKey]bool{}, testZoneNames)

	// Resume detecting: leave timer should be cancelled
	now = now.Add(1 * time.Second)
	events := pt.Update(map[PresenceKey]bool{key: true}, testZoneNames)
	if len(events) != 0 {
		t.Fatalf("expected 0 events when re-detecting, got %d", len(events))
	}

	// Verify still present
	present, _, _ := pt.GetPresence(key)
	if !present {
		t.Error("expected still present after re-detection")
	}

	// Stop detecting again. The leave timer restarts fresh.
	now = now.Add(1 * time.Second)
	pt.Update(map[PresenceKey]bool{}, testZoneNames)

	// 29 seconds later: should not have left yet
	now = now.Add(29 * time.Second)
	events = pt.Update(map[PresenceKey]bool{}, testZoneNames)
	if len(events) != 0 {
		t.Fatalf("expected 0 events before leave debounce, got %d", len(events))
	}

	// 1 more second (30s total since second stop): should leave
	now = now.Add(1 * time.Second)
	events = pt.Update(map[PresenceKey]bool{}, testZoneNames)
	if len(events) != 1 {
		t.Fatalf("expected 1 leave event, got %d", len(events))
	}
}

func TestPresence_GetPresence(t *testing.T) {
	pt := newTestTracker()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	pt.now = func() time.Time { return now }

	key := PresenceKey{ZoneID: 1, Label: "car"}

	// Not tracked yet
	present, _, _ := pt.GetPresence(key)
	if present {
		t.Error("expected not present before any detection")
	}

	// Enter zone
	pt.Update(map[PresenceKey]bool{key: true}, testZoneNames)
	now = now.Add(4 * time.Second)
	pt.Update(map[PresenceKey]bool{key: true}, testZoneNames)

	present, lastSeen, lastChanged := pt.GetPresence(key)
	if !present {
		t.Error("expected present after entering")
	}
	if lastSeen.IsZero() {
		t.Error("expected lastSeen to be set")
	}
	if lastChanged.IsZero() {
		t.Error("expected lastChanged to be set")
	}
}

func TestPresence_MultipleLabelsSameZone(t *testing.T) {
	pt := newTestTracker()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	pt.now = func() time.Time { return now }

	carKey := PresenceKey{ZoneID: 1, Label: "car"}
	truckKey := PresenceKey{ZoneID: 1, Label: "truck"}

	// Both car and truck in zone
	pt.Update(map[PresenceKey]bool{carKey: true, truckKey: true}, testZoneNames)
	now = now.Add(4 * time.Second)
	events := pt.Update(map[PresenceKey]bool{carKey: true, truckKey: true}, testZoneNames)

	if len(events) != 2 {
		t.Fatalf("expected 2 enter events, got %d", len(events))
	}

	// Remove truck only - first frame without truck starts leave timer
	now = now.Add(1 * time.Second)
	events = pt.Update(map[PresenceKey]bool{carKey: true}, testZoneNames)
	if len(events) != 0 {
		t.Fatalf("expected 0 events before leave debounce, got %d", len(events))
	}

	// 30 seconds later: truck leave debounce met
	now = now.Add(30 * time.Second)
	events = pt.Update(map[PresenceKey]bool{carKey: true}, testZoneNames)

	// Truck should leave, car should stay
	if len(events) != 1 {
		t.Fatalf("expected 1 leave event, got %d", len(events))
	}
	if events[0].Label != "truck" {
		t.Errorf("expected truck to leave, got %q", events[0].Label)
	}

	// Car should still be present
	present, _, _ := pt.GetPresence(carKey)
	if !present {
		t.Error("expected car still present")
	}
}

func TestPresence_AllPresence(t *testing.T) {
	pt := newTestTracker()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	pt.now = func() time.Time { return now }

	key := PresenceKey{ZoneID: 1, Label: "car"}

	// Enter
	pt.Update(map[PresenceKey]bool{key: true}, testZoneNames)
	now = now.Add(4 * time.Second)
	pt.Update(map[PresenceKey]bool{key: true}, testZoneNames)

	all := pt.AllPresence()
	if len(all) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(all))
	}
	if !all[key].Present {
		t.Error("expected present=true")
	}
}

func TestPresence_RapidTransitions(t *testing.T) {
	pt := newTestTracker()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	pt.now = func() time.Time { return now }

	key := PresenceKey{ZoneID: 1, Label: "car"}

	// Rapid on-off-on-off should NOT trigger enter
	for i := 0; i < 10; i++ {
		now = now.Add(1 * time.Second)
		events := pt.Update(map[PresenceKey]bool{key: true}, testZoneNames)
		if len(events) != 0 {
			t.Fatalf("unexpected event during rapid transitions at iteration %d", i)
		}
		now = now.Add(500 * time.Millisecond)
		pt.Update(map[PresenceKey]bool{}, testZoneNames)
	}

	// Should still not be present
	present, _, _ := pt.GetPresence(key)
	if present {
		t.Error("expected not present during rapid transitions")
	}
}

// Presence now only sees an object on frames where a detection matched it. A
// parked car is re-detected once per stationaryReconfirmInterval, so the leave
// debounce must outlast that cadence, with room for a reconfirm to miss. If it
// does not, every parked car "leaves" between reconfirmations.
func TestPresence_ParkedObjectSurvivesReconfirmCadence(t *testing.T) {
	pt := NewPresenceTracker() // production debounce values, not the test helper's
	clock := time.Date(2026, 7, 8, 8, 0, 0, 0, time.UTC)
	pt.now = func() time.Time { return clock }

	zones := map[int]string{1: "driveway"}
	key := PresenceKey{ZoneID: 1, Label: "car"}
	detected := map[PresenceKey]bool{key: true}
	empty := map[PresenceKey]bool{}

	const frame = 200 * time.Millisecond
	var leaves int

	// Arrival: the car is moving, so motion-gated detection fires on every frame.
	for elapsed := time.Duration(0); elapsed < 5*time.Second; elapsed += frame {
		pt.Update(detected, zones)
		clock = clock.Add(frame)
	}
	if present, _, _ := pt.GetPresence(key); !present {
		t.Fatalf("car detected on every frame for 5s: presence = false, want true")
	}

	// Parked: motion stops, so a detection only lands at each reconfirm. The
	// reconfirm fires on the first frame at or after the interval, so gaps are a
	// little over 30s, and YOLO periodically fails to re-see a still car in poor
	// light. The debounce has to ride out a missed reconfirmation; every third is
	// dropped here. Production showed 195 spurious sub-10-minute "left" stretches
	// in one week from exactly this.
	reconfirms := 0
	for elapsed := time.Duration(0); elapsed < 10*time.Minute; elapsed += frame {
		matches := empty
		if elapsed > 0 && elapsed%stationaryReconfirmInterval == 0 {
			reconfirms++
			if reconfirms%3 != 0 { // every third reconfirmation misses the car
				matches = detected
			}
		}
		for _, ev := range pt.Update(matches, zones) {
			if ev.Type == "zone_leave" {
				leaves++
			}
		}
		clock = clock.Add(frame)
	}

	if leaves != 0 {
		t.Errorf("parked car re-detected every %v: got %d zone_leave events, want 0",
			stationaryReconfirmInterval, leaves)
	}
	if present, _, _ := pt.GetPresence(key); !present {
		t.Errorf("parked car re-detected every %v: presence = false, want true",
			stationaryReconfirmInterval)
	}
}

// When detections stop entirely the zone must stop claiming presence, and it must
// do so promptly. It reports Unknown rather than a leave, because nothing observed
// the object depart.
func TestPresence_ReportsUnknownOnceDetectionsStop(t *testing.T) {
	pt := NewPresenceTracker()
	clock := time.Date(2026, 7, 8, 8, 0, 0, 0, time.UTC)
	pt.now = func() time.Time { return clock }

	zones := map[int]string{1: "driveway"}
	key := PresenceKey{ZoneID: 1, Label: "car"}
	detected := map[PresenceKey]bool{key: true}
	empty := map[PresenceKey]bool{}

	const frame = 200 * time.Millisecond
	for elapsed := time.Duration(0); elapsed < 10*time.Second; elapsed += frame {
		pt.Update(detected, zones)
		clock = clock.Add(frame)
	}
	if present, _, _ := pt.GetPresence(key); !present {
		t.Fatalf("car detected for 10s: presence = false, want true")
	}

	var leftAfter time.Duration
	for elapsed := time.Duration(0); elapsed < 10*time.Minute; elapsed += frame {
		for _, ev := range pt.Update(empty, zones) {
			if ev.Type == EventZoneLeave {
				t.Fatalf("detections stopped: emitted zone_leave, but nothing saw the object depart")
			}
			if ev.Type == EventZoneUnknown && leftAfter == 0 {
				leftAfter = elapsed
			}
		}
		clock = clock.Add(frame)
	}

	if leftAfter == 0 {
		t.Fatalf("detections stopped: no zone_unknown emitted within 10 minutes")
	}
	if leftAfter > 3*time.Minute {
		t.Errorf("detections stopped: zone_unknown took %v, want within 3m", leftAfter)
	}
	if status, _ := pt.GetState(key); status != PresenceUnknown {
		t.Errorf("detections stopped: state = %v, want unknown", status)
	}
}

// --- step 3: a zone must distinguish "it left" from "I stopped seeing it" ---

func obsIn(key PresenceKey, trackIDs ...int) (map[PresenceKey]map[int]bool, map[int]bool) {
	inZone := map[PresenceKey]map[int]bool{key: {}}
	detected := map[int]bool{}
	for _, id := range trackIDs {
		inZone[key][id] = true
		detected[id] = true
	}
	return inZone, detected
}

func enterZone(t *testing.T, pt *PresenceTracker, clock *time.Time, key PresenceKey, trackID int) {
	t.Helper()
	inZone, detected := obsIn(key, trackID)
	pt.UpdateObserved(inZone, detected, testZoneNames)
	*clock = clock.Add(4 * time.Second)
	pt.UpdateObserved(inZone, detected, testZoneNames)
	if state, _ := pt.GetState(key); state != PresencePresent {
		t.Fatalf("setup: expected present after enter debounce, got %v", state)
	}
}

// The car drives off the driveway: its track is still detected, just no longer
// inside the zone. That is a departure and must be reported promptly.
func TestPresence_TrackExitsZoneWhileDetected_Leaves(t *testing.T) {
	pt := NewPresenceTracker()
	clock := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	pt.now = func() time.Time { return clock }
	key := PresenceKey{ZoneID: 1, Label: "car"}

	enterZone(t, pt, &clock, key, 7)

	// Still detected (driving down the street), but out of the zone.
	var events []PresenceEvent
	for elapsed := time.Duration(0); elapsed < 10*time.Second; elapsed += time.Second {
		events = append(events, pt.UpdateObserved(
			map[PresenceKey]map[int]bool{}, map[int]bool{7: true}, testZoneNames)...)
		clock = clock.Add(time.Second)
	}

	var leaves int
	for _, ev := range events {
		if ev.Type == "zone_leave" {
			leaves++
		}
		if ev.Type == "zone_unknown" {
			t.Errorf("track exited the zone while still detected: got zone_unknown, want zone_leave")
		}
	}
	if leaves != 1 {
		t.Errorf("track exited the zone while still detected: got %d zone_leave events, want 1", leaves)
	}
	if state, _ := pt.GetState(key); state != PresenceAbsent {
		t.Errorf("after exiting the zone: state = %v, want absent", state)
	}
}

// Night falls and the detector loses the parked car. Nothing observed it leave.
// Reporting "left" would assert a departure from an absence of evidence, which is
// what made the driveway sensor untrustworthy. It must say it does not know.
func TestPresence_TrackLostInPlace_ReportsUnknownNotLeave(t *testing.T) {
	pt := NewPresenceTracker()
	clock := time.Date(2026, 7, 8, 23, 0, 0, 0, time.UTC)
	pt.now = func() time.Time { return clock }
	key := PresenceKey{ZoneID: 1, Label: "car"}

	enterZone(t, pt, &clock, key, 7)

	// Track 7 is no longer detected anywhere: not in the zone, not outside it.
	var events []PresenceEvent
	for elapsed := time.Duration(0); elapsed < 5*time.Minute; elapsed += time.Second {
		events = append(events, pt.UpdateObserved(
			map[PresenceKey]map[int]bool{}, map[int]bool{}, testZoneNames)...)
		clock = clock.Add(time.Second)
	}

	var unknowns, leaves int
	for _, ev := range events {
		switch ev.Type {
		case "zone_unknown":
			unknowns++
		case "zone_leave":
			leaves++
		}
	}
	if leaves != 0 {
		t.Errorf("track lost in place: got %d zone_leave events, want 0 (absence of evidence is not a departure)", leaves)
	}
	if unknowns != 1 {
		t.Errorf("track lost in place: got %d zone_unknown events, want exactly 1", unknowns)
	}
	if state, _ := pt.GetState(key); state != PresenceUnknown {
		t.Errorf("after losing the track in place: state = %v, want unknown", state)
	}
}

// Morning: the detector sees the car again. Unknown resolves back to present.
func TestPresence_UnknownRecoversToPresentWhenSeenAgain(t *testing.T) {
	pt := NewPresenceTracker()
	clock := time.Date(2026, 7, 8, 23, 0, 0, 0, time.UTC)
	pt.now = func() time.Time { return clock }
	key := PresenceKey{ZoneID: 1, Label: "car"}

	enterZone(t, pt, &clock, key, 7)
	for elapsed := time.Duration(0); elapsed < 5*time.Minute; elapsed += time.Second {
		pt.UpdateObserved(map[PresenceKey]map[int]bool{}, map[int]bool{}, testZoneNames)
		clock = clock.Add(time.Second)
	}
	if state, _ := pt.GetState(key); state != PresenceUnknown {
		t.Fatalf("setup: expected unknown, got %v", state)
	}

	// A new track for the same car appears in the zone.
	inZone, detected := obsIn(key, 12)
	var events []PresenceEvent
	for elapsed := time.Duration(0); elapsed < 5*time.Second; elapsed += time.Second {
		events = append(events, pt.UpdateObserved(inZone, detected, testZoneNames)...)
		clock = clock.Add(time.Second)
	}

	var enters int
	for _, ev := range events {
		if ev.Type == "zone_enter" {
			enters++
		}
	}
	if enters != 1 {
		t.Errorf("car seen again after unknown: got %d zone_enter events, want 1", enters)
	}
	if state, _ := pt.GetState(key); state != PresencePresent {
		t.Errorf("car seen again after unknown: state = %v, want present", state)
	}
}
