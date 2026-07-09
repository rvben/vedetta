package detect

import (
	"math"
	"sort"
)

// TrackState represents the lifecycle state of a tracked object.
type TrackState string

const (
	TrackTentative TrackState = "tentative"
	TrackConfirmed TrackState = "confirmed"
	TrackDeleted   TrackState = "deleted"
)

// TrackedObject is the external representation of a confirmed track.
type TrackedObject struct {
	TrackID    int
	Label      string
	Score      float32
	Box        [4]int // x1, y1, x2, y2
	State      string
	FramesSeen int
	// Moved reports whether the centroid has ever traveled more than
	// defaultMovedRatio of the box size from where the track was first seen.
	// It stays false for an object that has only sat still (a parked vehicle),
	// which lets the camera categorize its events as low-priority detections
	// rather than alerts.
	Moved bool
	// Detected reports whether a detection matched this track on the most recent
	// update. A confirmed track keeps being returned while it coasts through
	// frames without a detection, and a stationary one coasts for 50x the normal
	// budget. Callers that must distinguish observation from belief, such as zone
	// presence, have to consult this rather than the track's existence.
	Detected bool
}

// track is the internal state for a single tracked object.
type track struct {
	id          int
	label       string
	box         [4]int
	prevBox     [4]int // last matched box (for stationarity IoU comparison)
	score       float32
	age         int
	hits        int
	disappeared int
	state       TrackState

	// birthBox is the box at track creation; moved latches true once the
	// centroid travels past defaultMovedRatio * max(box dim) from it. Used to
	// distinguish an object that arrived/traveled from one that has only ever
	// sat still.
	birthBox [4]int
	moved    bool

	// Stationarity bookkeeping. A track is stationary while
	// stationaryHits >= Tracker.stationaryThreshold AND its centroid hasn't
	// drifted past Tracker.stationaryDriftRatio × max(box dims) from the
	// streak anchor. lastWasStationary captures that decision at the most
	// recent matched update so it can be consulted during miss-streak aging.
	stationaryHits      int
	stationaryAnchor    [4]int // box at the start of the current high-IoU streak
	hasStationaryAnchor bool
	lastWasStationary   bool
}

// Tracker matches detections across frames using IoU to maintain stable object identities.
//
// A track that has been matched to a near-identical bounding box for several
// consecutive updates is treated as "stationary" and granted a much longer
// disappearance budget. This prevents motion-gated detection gaps (a parked
// car only re-detected when something nearby moves) from churning the
// TrackID and triggering duplicate events for the same object.
type Tracker struct {
	maxDisappeared           int
	stationaryMaxDisappeared int
	minHits                  int
	stationaryThreshold      int     // consecutive high-IoU matches needed to promote
	stationaryIoU            float64 // per-frame IoU above which a match counts toward the streak
	stationaryDriftRatio     float64 // cumulative drift cap (× max box dim) before streak resets
	nextID                   int
	tracks                   []*track
}

// Default stationarity tuning. Sized for a 5 fps detect loop with YOLO
// gated behind motion: a parked car only generates matched updates while
// motion is qualifying nearby, so promotion accumulates across the matched
// frames of the car's lifetime (nil frames preserve the streak) rather than
// requiring an uninterrupted run of detections.
//
// Stationarity is judged by centroid stability, not bounding-box overlap.
// YOLO's box for a large vehicle wobbles in size frame to frame (it includes
// or omits part of the body as lighting and occlusion change), which drops
// the per-frame IoU of a perfectly still van to ~0.84. The IoU floor
// (defaultStationaryIoU) therefore only rejects gross position jumps to a
// different object; the 0.3 drift ratio is the real discriminator, resetting
// the streak once the centroid creeps past 0.3 x max(box dim) from the streak
// anchor. That catches a person walking 3 px/frame within 11 frames while
// tolerating box-size wobble on a stationary vehicle.
//
// 10 matched hits promotes; stationaryMaxDisappeared = 50 x maxDisappeared
// keeps the parked-car identity alive for ~5 minutes at 5 fps (maxDisappeared
// =30), covering plausible motion-quiet windows (wind, brief obstructions,
// short YOLO stalls).
const (
	defaultStationaryThresholdHits = 10
	defaultStationaryIoU           = 0.5
	defaultStationaryDriftRatio    = 0.3
	defaultStationaryMaxMultiplier = 50
	// defaultMovedRatio is the cumulative centroid travel (as a fraction of the
	// box's larger dimension) from where a track was first seen, above which the
	// object counts as having "moved". Sized above realistic box-size wobble on
	// a parked vehicle (~0.06) and below the displacement of an object actually
	// traveling through the scene.
	defaultMovedRatio = 0.15
)

// NewTracker creates a tracker with the given decay parameters and the
// default stationary preservation policy.
//
// maxDisappeared: frames before a non-stationary track is deleted.
// minHits: consecutive frames before a tentative track is confirmed.
func NewTracker(maxDisappeared, minHits int) *Tracker {
	return NewTrackerWithStationary(
		maxDisappeared,
		minHits,
		maxDisappeared*defaultStationaryMaxMultiplier,
		defaultStationaryThresholdHits,
		defaultStationaryIoU,
		defaultStationaryDriftRatio,
	)
}

// NewTrackerWithStationary builds a tracker with explicit stationarity tuning.
// Tests use this to dial down the timeouts and thresholds for fast scenarios;
// production wires up the tuned defaults via NewTracker.
func NewTrackerWithStationary(
	maxDisappeared, minHits int,
	stationaryMaxDisappeared, stationaryThreshold int,
	stationaryIoU, stationaryDriftRatio float64,
) *Tracker {
	return &Tracker{
		maxDisappeared:           maxDisappeared,
		stationaryMaxDisappeared: stationaryMaxDisappeared,
		minHits:                  minHits,
		stationaryThreshold:      stationaryThreshold,
		stationaryIoU:            stationaryIoU,
		stationaryDriftRatio:     stationaryDriftRatio,
		nextID:                   1,
	}
}

// Update processes a new set of detections and returns confirmed tracked objects.
// It also returns tracks that just transitioned to confirmed or deleted state
// so the caller can emit start/end events.
func (t *Tracker) Update(detections []Detection) []TrackedObject {
	// Remove previously deleted tracks
	alive := t.tracks[:0]
	for _, tr := range t.tracks {
		if tr.state != TrackDeleted {
			alive = append(alive, tr)
		}
	}
	t.tracks = alive

	// Age all tracks
	for _, tr := range t.tracks {
		tr.age++
	}

	// If no existing tracks, create new ones from all detections
	if len(t.tracks) == 0 {
		for _, d := range detections {
			t.tracks = append(t.tracks, t.newTrack(d))
		}
		return t.confirmedObjects()
	}

	// If no detections, increment disappeared for all tracks
	if len(detections) == 0 {
		for _, tr := range t.tracks {
			tr.disappeared++
			if tr.disappeared > t.effectiveMaxDisappeared(tr) {
				tr.state = TrackDeleted
			}
		}
		return t.confirmedObjects()
	}

	// Build IoU cost matrix and perform greedy assignment
	type assignment struct {
		trackIdx     int
		detectionIdx int
		iou          float64
	}

	var pairs []assignment
	for ti, tr := range t.tracks {
		for di, d := range detections {
			v := iou(tr.box, d.Box)
			if v > 0 {
				pairs = append(pairs, assignment{ti, di, v})
			}
		}
	}

	// Sort by IoU descending for greedy matching
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].iou > pairs[j].iou
	})

	matchedTracks := make(map[int]bool)
	matchedDets := make(map[int]bool)

	for _, p := range pairs {
		if matchedTracks[p.trackIdx] || matchedDets[p.detectionIdx] {
			continue
		}
		matchedTracks[p.trackIdx] = true
		matchedDets[p.detectionIdx] = true

		tr := t.tracks[p.trackIdx]
		d := detections[p.detectionIdx]
		t.applyMatch(tr, d)
	}

	// Unmatched tracks: increment disappeared
	for ti, tr := range t.tracks {
		if !matchedTracks[ti] {
			tr.disappeared++
			if tr.disappeared > t.effectiveMaxDisappeared(tr) {
				tr.state = TrackDeleted
			}
		}
	}

	// Unmatched detections: create new tentative tracks
	for di, d := range detections {
		if !matchedDets[di] {
			t.tracks = append(t.tracks, t.newTrack(d))
		}
	}

	return t.confirmedObjects()
}

// applyMatch updates a track with a freshly-matched detection and refreshes
// its stationarity state. The stationary streak grows when the match has
// high IoU with the *previous* matched box AND the cumulative drift from
// the streak anchor stays below the configured ratio. Either guard failing
// resets the streak — slow drift over time is treated as motion even if
// per-frame IoU is high.
func (t *Tracker) applyMatch(tr *track, d Detection) {
	prevIoU := iou(tr.prevBox, d.Box)
	streakActive := prevIoU > t.stationaryIoU

	if streakActive {
		if !tr.hasStationaryAnchor {
			tr.stationaryAnchor = tr.prevBox
			tr.hasStationaryAnchor = true
			tr.stationaryHits = 0
		}
		drift := centroidDistance(tr.stationaryAnchor, d.Box)
		boxDim := math.Max(float64(d.Box[2]-d.Box[0]), float64(d.Box[3]-d.Box[1]))
		if boxDim <= 0 || drift > t.stationaryDriftRatio*boxDim {
			tr.stationaryHits = 0
			tr.hasStationaryAnchor = false
		} else {
			tr.stationaryHits++
		}
	} else {
		tr.stationaryHits = 0
		tr.hasStationaryAnchor = false
	}

	tr.lastWasStationary = tr.stationaryHits >= t.stationaryThreshold

	tr.box = d.Box
	if !tr.moved {
		boxDim := math.Max(float64(d.Box[2]-d.Box[0]), float64(d.Box[3]-d.Box[1]))
		if boxDim > 0 && centroidDistance(tr.birthBox, d.Box) > defaultMovedRatio*boxDim {
			tr.moved = true
		}
	}
	tr.prevBox = d.Box
	tr.score = d.Score
	tr.label = d.Label
	tr.hits++
	tr.disappeared = 0
	if tr.state == TrackTentative && tr.hits >= t.minHits {
		tr.state = TrackConfirmed
	}
}

// effectiveMaxDisappeared returns the disappearance budget that should
// apply to this track on the current Update. Stationary tracks earn the
// longer budget; everything else uses the standard one.
func (t *Tracker) effectiveMaxDisappeared(tr *track) int {
	if tr.lastWasStationary {
		return t.stationaryMaxDisappeared
	}
	return t.maxDisappeared
}

// HasStationaryConfirmed reports whether any confirmed track is currently
// classified as stationary. A motion-gated detect loop uses this to decide
// whether to periodically re-run detection during quiet periods: re-confirming
// a parked object keeps it a single tracked object (one event) for its whole
// dwell instead of letting it age out and re-detect as a new track.
func (t *Tracker) HasStationaryConfirmed() bool {
	for _, tr := range t.tracks {
		if tr.state == TrackConfirmed && tr.lastWasStationary {
			return true
		}
	}
	return false
}

// DeletedTracks returns tracks that were just marked deleted in the last Update call.
func (t *Tracker) DeletedTracks() []TrackedObject {
	var result []TrackedObject
	for _, tr := range t.tracks {
		if tr.state == TrackDeleted {
			result = append(result, TrackedObject{
				TrackID:    tr.id,
				Label:      tr.label,
				Score:      tr.score,
				Box:        tr.box,
				State:      string(TrackDeleted),
				FramesSeen: tr.hits,
			})
		}
	}
	return result
}

func (t *Tracker) newTrack(d Detection) *track {
	tr := &track{
		id:       t.nextID,
		label:    d.Label,
		box:      d.Box,
		prevBox:  d.Box,
		birthBox: d.Box,
		score:    d.Score,
		age:      1,
		hits:     1,
		state:    TrackTentative,
	}
	t.nextID++
	if tr.hits >= t.minHits {
		tr.state = TrackConfirmed
	}
	return tr
}

func (t *Tracker) confirmedObjects() []TrackedObject {
	var result []TrackedObject
	for _, tr := range t.tracks {
		if tr.state == TrackConfirmed {
			result = append(result, TrackedObject{
				TrackID:    tr.id,
				Label:      tr.label,
				Score:      tr.score,
				Box:        tr.box,
				State:      string(tr.state),
				FramesSeen: tr.hits,
				Moved:      tr.moved,
				Detected:   tr.disappeared == 0,
			})
		}
	}
	return result
}

// centroidDistance returns the Euclidean distance between the centroids of
// two boxes (x1,y1,x2,y2).
func centroidDistance(a, b [4]int) float64 {
	cx1 := float64(a[0]+a[2]) / 2
	cy1 := float64(a[1]+a[3]) / 2
	cx2 := float64(b[0]+b[2]) / 2
	cy2 := float64(b[1]+b[3]) / 2
	dx := cx1 - cx2
	dy := cy1 - cy2
	return math.Sqrt(dx*dx + dy*dy)
}
