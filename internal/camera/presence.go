package camera

import (
	"log/slog"
	"sync"
	"time"
)

// PresenceKey identifies a unique zone+label combination for presence tracking.
type PresenceKey struct {
	ZoneID int
	Label  string
}

// PresenceStatus is what a zone knows about a label.
//
// Unknown is not a shade of Absent. A zone reports Unknown when the objects that
// established its presence stopped being detected anywhere, without ever being
// seen to leave: the detector loses a parked car in the dark, or the camera goes
// blind. Collapsing that into Absent asserts a departure from an absence of
// evidence, which is how the driveway sensor came to claim the car had left while
// it sat on the drive.
type PresenceStatus int

const (
	PresenceAbsent PresenceStatus = iota
	PresencePresent
	PresenceUnknown
)

func (s PresenceStatus) String() string {
	switch s {
	case PresencePresent:
		return "present"
	case PresenceUnknown:
		return "unknown"
	default:
		return "absent"
	}
}

// Presence event types.
const (
	EventZoneEnter   = "zone_enter"
	EventZoneLeave   = "zone_leave"
	EventZoneUnknown = "zone_unknown"
)

// presenceState tracks the internal state for a single zone+label presence.
type presenceState struct {
	status      PresenceStatus
	lastSeen    time.Time
	lastChanged time.Time
	// members are the track IDs that currently establish this zone's presence.
	// Whether they are still detected elsewhere is what separates a departure
	// from a loss.
	members map[int]bool
	// Debounce state
	enteringSince time.Time // first in-zone detection after absence (zero if not entering)
	exitingSince  time.Time // members left the zone but are still detected (zero if not exiting)
	lostSince     time.Time // members stopped being detected at all (zero if not lost)
}

// PresenceEvent represents a change in presence state.
type PresenceEvent struct {
	ZoneID   int
	ZoneName string
	Label    string
	Type     string // EventZoneEnter, EventZoneLeave or EventZoneUnknown
	Time     time.Time
}

// PresenceTracker manages presence state for zones with track_presence enabled.
type PresenceTracker struct {
	mu            sync.Mutex
	states        map[PresenceKey]*presenceState
	debounceEnter time.Duration
	debounceExit  time.Duration
	debounceLeave time.Duration    // retained: how long a lost object waits before Unknown
	now           func() time.Time // injectable clock for testing

	shimIDs  map[PresenceKey]int // synthetic track IDs for the legacy Update path
	nextShim int
}

// lostReconfirms is how many stationary reconfirmations a present object may miss
// before the zone admits it no longer knows.
//
// Presence is fed only by frames where a detection matched (see matchZones), and
// a parked object is re-detected just once per stationaryReconfirmInterval. The
// timeout therefore has to outlast that cadence: at 1x it is a coin flip, because
// the reconfirm fires on the first frame at or after the interval and detection
// periodically fails to re-see a still car in poor light. 3x rides out a missed
// reconfirmation.
const lostReconfirms = 3

// NewPresenceTracker creates a new PresenceTracker with default debounce timings.
func NewPresenceTracker() *PresenceTracker {
	return &PresenceTracker{
		states:        make(map[PresenceKey]*presenceState),
		debounceEnter: 3 * time.Second,
		debounceExit:  3 * time.Second,
		debounceLeave: lostReconfirms * stationaryReconfirmInterval,
		now:           time.Now,
		shimIDs:       make(map[PresenceKey]int),
	}
}

// Update is the legacy entry point: it knows only which zones hold a detection of
// each label, so an object that stops matching a zone is indistinguishable from
// one that stopped being detected. It therefore treats every disappearance as a
// loss, never as a departure. Callers that can see the tracks should use
// UpdateObserved.
func (pt *PresenceTracker) Update(zoneMatches map[PresenceKey]bool, zones map[int]string) []PresenceEvent {
	pt.mu.Lock()
	inZone := make(map[PresenceKey]map[int]bool, len(zoneMatches))
	detected := make(map[int]bool, len(zoneMatches))
	for key, matched := range zoneMatches {
		if !matched {
			continue
		}
		id, ok := pt.shimIDs[key]
		if !ok {
			pt.nextShim++
			id = -pt.nextShim // negative ids cannot collide with real track ids
			pt.shimIDs[key] = id
		}
		inZone[key] = map[int]bool{id: true}
		detected[id] = true
	}
	pt.mu.Unlock()

	return pt.UpdateObserved(inZone, detected, zones)
}

// UpdateObserved processes one frame of observations and returns presence changes.
//
// inZone maps each zone+label to the track IDs a detection placed inside that zone
// on this frame. detected holds every track ID a detection matched this frame,
// wherever it was. The difference between the two is the whole point: a member
// track missing from its zone but present in detected walked out of the zone, and
// a member missing from both was simply lost.
func (pt *PresenceTracker) UpdateObserved(
	inZone map[PresenceKey]map[int]bool,
	detected map[int]bool,
	zones map[int]string,
) []PresenceEvent {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	now := pt.now()
	var events []PresenceEvent

	emit := func(key PresenceKey, evType string) {
		events = append(events, PresenceEvent{
			ZoneID:   key.ZoneID,
			ZoneName: zones[key.ZoneID],
			Label:    key.Label,
			Type:     evType,
			Time:     now,
		})
	}

	// Objects seen inside their zone this frame.
	for key, tracks := range inZone {
		if len(tracks) == 0 {
			continue
		}
		state, exists := pt.states[key]
		if !exists {
			state = &presenceState{}
			pt.states[key] = state
		}

		state.lastSeen = now
		state.members = tracks
		state.exitingSince = time.Time{}
		state.lostSince = time.Time{}

		if state.status == PresencePresent {
			continue
		}
		if state.enteringSince.IsZero() {
			state.enteringSince = now
			continue
		}
		if now.Sub(state.enteringSince) >= pt.debounceEnter {
			state.status = PresencePresent
			state.lastChanged = now
			state.enteringSince = time.Time{}
			emit(key, EventZoneEnter)
			slog.Info("zone presence entered", "zone", zones[key.ZoneID], "label", key.Label)
		}
	}

	// Drop absent states nothing has referenced for a while. Unknown states are
	// kept: the zone is still asserting that it does not know, and forgetting
	// would silently downgrade that to absent.
	for key, state := range pt.states {
		if state.status == PresenceAbsent && !state.lastSeen.IsZero() && now.Sub(state.lastSeen) > 5*time.Minute {
			delete(pt.states, key)
		}
	}

	// Objects not seen inside their zone this frame.
	for key, state := range pt.states {
		if len(inZone[key]) > 0 {
			continue
		}
		state.enteringSince = time.Time{}

		if state.status != PresencePresent {
			continue
		}

		if membersStillDetected(state.members, detected) {
			// Seen, just not here: it walked out of the zone.
			state.lostSince = time.Time{}
			if state.exitingSince.IsZero() {
				state.exitingSince = now
				continue
			}
			if now.Sub(state.exitingSince) >= pt.debounceExit {
				state.status = PresenceAbsent
				state.lastChanged = now
				state.exitingSince = time.Time{}
				state.members = nil
				emit(key, EventZoneLeave)
				slog.Info("zone presence left", "zone", zones[key.ZoneID], "label", key.Label)
			}
			continue
		}

		// Not seen anywhere. We do not know whether it left.
		state.exitingSince = time.Time{}
		if state.lostSince.IsZero() {
			state.lostSince = now
			continue
		}
		if now.Sub(state.lostSince) >= pt.debounceLeave {
			state.status = PresenceUnknown
			state.lastChanged = now
			state.lostSince = time.Time{}
			state.members = nil
			emit(key, EventZoneUnknown)
			slog.Info("zone presence unknown",
				"zone", zones[key.ZoneID],
				"label", key.Label,
				"reason", "tracks lost without being seen to leave",
			)
		}
	}

	return events
}

// membersStillDetected reports whether any track that established a zone's
// presence was matched by a detection this frame, wherever it now sits.
func membersStillDetected(members map[int]bool, detected map[int]bool) bool {
	for id := range members {
		if detected[id] {
			return true
		}
	}
	return false
}

// GetState returns the current status for a key, and when it last changed.
func (pt *PresenceTracker) GetState(key PresenceKey) (PresenceStatus, time.Time) {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	if state, ok := pt.states[key]; ok {
		return state.status, state.lastChanged
	}
	return PresenceAbsent, time.Time{}
}

// GetPresence returns the current presence state for a given key.
func (pt *PresenceTracker) GetPresence(key PresenceKey) (present bool, lastSeen, lastChanged time.Time) {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	if state, ok := pt.states[key]; ok {
		return state.status == PresencePresent, state.lastSeen, state.lastChanged
	}
	return false, time.Time{}, time.Time{}
}

// AllPresence returns all current presence states.
func (pt *PresenceTracker) AllPresence() map[PresenceKey]ZonePresence {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	result := make(map[PresenceKey]ZonePresence, len(pt.states))
	for key, state := range pt.states {
		result[key] = ZonePresence{
			ZoneID:      key.ZoneID,
			Label:       key.Label,
			Present:     state.status == PresencePresent,
			LastSeen:    state.lastSeen,
			LastChanged: state.lastChanged,
		}
	}
	return result
}
