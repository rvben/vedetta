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

// presenceState tracks the internal state for a single zone+label presence.
type presenceState struct {
	present     bool
	lastSeen    time.Time
	lastChanged time.Time
	// Debounce state
	enteringSince time.Time // first detection after absence (zero if not entering)
	leavingSince  time.Time // last detection before absence started (zero if not leaving)
}

// PresenceEvent represents a change in presence state.
type PresenceEvent struct {
	ZoneID   int
	ZoneName string
	Label    string
	Type     string // "zone_enter" or "zone_leave"
	Time     time.Time
}

// PresenceTracker manages presence state for zones with track_presence enabled.
type PresenceTracker struct {
	mu            sync.Mutex
	states        map[PresenceKey]*presenceState
	debounceEnter time.Duration
	debounceLeave time.Duration
	now           func() time.Time // injectable clock for testing
}

// debounceLeaveReconfirms is how many stationary reconfirmations a present object
// may miss before it counts as gone.
//
// Presence is fed only by frames where a detection matched (see matchZones), and
// a parked object is re-detected just once per stationaryReconfirmInterval. The
// leave debounce therefore has to outlast that cadence: at 1x it is a coin flip,
// because the reconfirm fires on the first frame at or after the interval and
// YOLO periodically fails to re-see a still car in poor light. 3x rides out a
// missed reconfirmation and still reports a real departure inside 90 seconds.
const debounceLeaveReconfirms = 3

// NewPresenceTracker creates a new PresenceTracker with default debounce timings.
func NewPresenceTracker() *PresenceTracker {
	return &PresenceTracker{
		states:        make(map[PresenceKey]*presenceState),
		debounceEnter: 3 * time.Second,
		debounceLeave: debounceLeaveReconfirms * stationaryReconfirmInterval,
		now:           time.Now,
	}
}

// Update processes current zone matches and returns presence change events.
// zoneMatches maps PresenceKey to true if a detection of that label was seen in that zone.
// zones provides name lookup for event generation.
func (pt *PresenceTracker) Update(zoneMatches map[PresenceKey]bool, zones map[int]string) []PresenceEvent {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	now := pt.now()
	var events []PresenceEvent

	// Process all keys that have current detections
	for key, detected := range zoneMatches {
		state, exists := pt.states[key]
		if !exists {
			state = &presenceState{}
			pt.states[key] = state
		}

		if detected {
			state.lastSeen = now
			// Cancel any pending leave timer
			state.leavingSince = time.Time{}

			if !state.present {
				// Start or continue enter timer
				if state.enteringSince.IsZero() {
					state.enteringSince = now
				} else if now.Sub(state.enteringSince) >= pt.debounceEnter {
					// Debounce period met, transition to PRESENT
					state.present = true
					state.lastChanged = now
					state.enteringSince = time.Time{}
					events = append(events, PresenceEvent{
						ZoneID:   key.ZoneID,
						ZoneName: zones[key.ZoneID],
						Label:    key.Label,
						Type:     "zone_enter",
						Time:     now,
					})
					slog.Info("zone presence entered",
						"zone", zones[key.ZoneID],
						"label", key.Label,
					)
				}
			}
		}
	}

	// Garbage-collect stale entries: not present and not seen for >5 minutes
	for key, state := range pt.states {
		if !state.present && !state.lastSeen.IsZero() && now.Sub(state.lastSeen) > 5*time.Minute {
			delete(pt.states, key)
			continue
		}
	}

	// Check all tracked states for leave transitions
	for key, state := range pt.states {
		if zoneMatches[key] {
			continue // Still detected, skip
		}

		// Not detected this frame
		// Cancel any pending enter timer
		state.enteringSince = time.Time{}

		if state.present {
			// Start or continue leave timer
			if state.leavingSince.IsZero() {
				state.leavingSince = now
			} else if now.Sub(state.leavingSince) >= pt.debounceLeave {
				// Debounce period met, transition to NOT_PRESENT
				state.present = false
				state.lastChanged = now
				state.leavingSince = time.Time{}
				events = append(events, PresenceEvent{
					ZoneID:   key.ZoneID,
					ZoneName: zones[key.ZoneID],
					Label:    key.Label,
					Type:     "zone_leave",
					Time:     now,
				})
				slog.Info("zone presence left",
					"zone", zones[key.ZoneID],
					"label", key.Label,
				)
			}
		}
	}

	return events
}

// GetPresence returns the current presence state for a given key.
func (pt *PresenceTracker) GetPresence(key PresenceKey) (present bool, lastSeen, lastChanged time.Time) {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	if state, ok := pt.states[key]; ok {
		return state.present, state.lastSeen, state.lastChanged
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
			Present:     state.present,
			LastSeen:    state.lastSeen,
			LastChanged: state.lastChanged,
		}
	}
	return result
}
