package detect

import "testing"

func TestIoU_PerfectOverlap(t *testing.T) {
	a := [4]int{0, 0, 100, 100}
	v := iou(a, a)
	if v != 1.0 {
		t.Errorf("expected 1.0, got %f", v)
	}
}

func TestIoU_NoOverlap(t *testing.T) {
	a := [4]int{0, 0, 50, 50}
	b := [4]int{60, 60, 100, 100}
	v := iou(a, b)
	if v != 0 {
		t.Errorf("expected 0, got %f", v)
	}
}

func TestIoU_PartialOverlap(t *testing.T) {
	a := [4]int{0, 0, 100, 100}
	b := [4]int{50, 50, 150, 150}
	v := iou(a, b)
	// Intersection: 50*50 = 2500
	// Union: 10000 + 10000 - 2500 = 17500
	expected := 2500.0 / 17500.0
	if v < expected-0.001 || v > expected+0.001 {
		t.Errorf("expected ~%f, got %f", expected, v)
	}
}

func TestTracker_SingleObjectMoving(t *testing.T) {
	tr := NewTracker(3, 2)

	// Frame 1: object appears
	objs := tr.Update([]Detection{
		{Label: "person", Score: 0.9, Box: [4]int{10, 10, 60, 60}},
	})
	// minHits=2, so not yet confirmed
	if len(objs) != 0 {
		t.Errorf("frame 1: expected 0 confirmed, got %d", len(objs))
	}

	// Frame 2: same object, slightly moved (high IoU)
	objs = tr.Update([]Detection{
		{Label: "person", Score: 0.92, Box: [4]int{12, 12, 62, 62}},
	})
	if len(objs) != 1 {
		t.Fatalf("frame 2: expected 1 confirmed, got %d", len(objs))
	}
	if objs[0].TrackID != 1 {
		t.Errorf("expected TrackID 1, got %d", objs[0].TrackID)
	}
	if objs[0].Label != "person" {
		t.Errorf("expected label person, got %s", objs[0].Label)
	}

	// Frame 3: object moves further
	objs = tr.Update([]Detection{
		{Label: "person", Score: 0.88, Box: [4]int{15, 15, 65, 65}},
	})
	if len(objs) != 1 {
		t.Fatalf("frame 3: expected 1 confirmed, got %d", len(objs))
	}
	if objs[0].TrackID != 1 {
		t.Errorf("expected same TrackID 1, got %d", objs[0].TrackID)
	}
}

func TestTracker_MultipleObjects(t *testing.T) {
	tr := NewTracker(3, 1) // minHits=1 for immediate confirmation

	objs := tr.Update([]Detection{
		{Label: "person", Score: 0.9, Box: [4]int{10, 10, 60, 60}},
		{Label: "car", Score: 0.85, Box: [4]int{200, 200, 400, 400}},
	})
	if len(objs) != 2 {
		t.Fatalf("expected 2 confirmed, got %d", len(objs))
	}

	ids := map[int]string{}
	for _, o := range objs {
		ids[o.TrackID] = o.Label
	}

	// Both should have unique IDs
	if len(ids) != 2 {
		t.Errorf("expected 2 unique IDs, got %d", len(ids))
	}

	// Frame 2: both move slightly
	objs = tr.Update([]Detection{
		{Label: "person", Score: 0.91, Box: [4]int{12, 12, 62, 62}},
		{Label: "car", Score: 0.86, Box: [4]int{202, 202, 402, 402}},
	})
	if len(objs) != 2 {
		t.Fatalf("frame 2: expected 2 confirmed, got %d", len(objs))
	}

	// IDs should be preserved
	for _, o := range objs {
		if o.Label == "person" && o.TrackID != 1 {
			t.Errorf("person should keep TrackID 1, got %d", o.TrackID)
		}
		if o.Label == "car" && o.TrackID != 2 {
			t.Errorf("car should keep TrackID 2, got %d", o.TrackID)
		}
	}
}

func TestTracker_ObjectDisappearsAndReappears(t *testing.T) {
	tr := NewTracker(2, 1) // maxDisappeared=2, confirm after 1 hit

	// Frame 1: object appears
	objs := tr.Update([]Detection{
		{Label: "person", Score: 0.9, Box: [4]int{10, 10, 60, 60}},
	})
	if len(objs) != 1 {
		t.Fatalf("expected 1, got %d", len(objs))
	}
	originalID := objs[0].TrackID

	// Frame 2: no detections
	objs = tr.Update(nil)
	// Track still alive (disappeared=1, maxDisappeared=2)
	if len(objs) != 1 {
		t.Fatalf("frame 2: expected 1 (still alive), got %d", len(objs))
	}

	// Frame 3: still no detections
	objs = tr.Update(nil)
	// disappeared=2, still alive (deleted when > maxDisappeared)
	if len(objs) != 1 {
		t.Fatalf("frame 3: expected 1 (still alive at boundary), got %d", len(objs))
	}

	// Frame 4: still no detections -> disappeared=3 > maxDisappeared=2, deleted
	objs = tr.Update(nil)
	if len(objs) != 0 {
		t.Fatalf("frame 4: expected 0 (deleted), got %d", len(objs))
	}

	// Frame 5: object reappears in same location -> gets new ID
	objs = tr.Update([]Detection{
		{Label: "person", Score: 0.9, Box: [4]int{10, 10, 60, 60}},
	})
	if len(objs) != 1 {
		t.Fatalf("frame 5: expected 1, got %d", len(objs))
	}
	if objs[0].TrackID == originalID {
		t.Errorf("reappeared object should get new ID, but got same ID %d", originalID)
	}
}

func TestTracker_OverlappingObjectsDifferentClasses(t *testing.T) {
	tr := NewTracker(3, 1)

	// Two overlapping objects with different labels
	objs := tr.Update([]Detection{
		{Label: "person", Score: 0.9, Box: [4]int{10, 10, 100, 100}},
		{Label: "dog", Score: 0.8, Box: [4]int{30, 30, 120, 120}},
	})
	if len(objs) != 2 {
		t.Fatalf("expected 2, got %d", len(objs))
	}

	// Frame 2: both move
	objs = tr.Update([]Detection{
		{Label: "person", Score: 0.91, Box: [4]int{12, 12, 102, 102}},
		{Label: "dog", Score: 0.82, Box: [4]int{32, 32, 122, 122}},
	})
	if len(objs) != 2 {
		t.Fatalf("frame 2: expected 2, got %d", len(objs))
	}

	labels := map[string]bool{}
	for _, o := range objs {
		labels[o.Label] = true
	}
	if !labels["person"] || !labels["dog"] {
		t.Errorf("expected both person and dog, got %v", labels)
	}
}

func TestTracker_NoDetectionsFrame(t *testing.T) {
	tr := NewTracker(5, 1)

	// Create two tracks
	tr.Update([]Detection{
		{Label: "person", Score: 0.9, Box: [4]int{10, 10, 60, 60}},
		{Label: "car", Score: 0.85, Box: [4]int{200, 200, 400, 400}},
	})

	// Empty frame
	objs := tr.Update(nil)
	// Both should still be confirmed (disappeared=1, maxDisappeared=5)
	if len(objs) != 2 {
		t.Errorf("expected 2 tracks still alive, got %d", len(objs))
	}
}

func TestTracker_MinHitsConfirmation(t *testing.T) {
	tr := NewTracker(3, 3) // Requires 3 consecutive hits

	// Frame 1
	objs := tr.Update([]Detection{
		{Label: "person", Score: 0.9, Box: [4]int{10, 10, 60, 60}},
	})
	if len(objs) != 0 {
		t.Errorf("frame 1: should not be confirmed yet, got %d", len(objs))
	}

	// Frame 2
	objs = tr.Update([]Detection{
		{Label: "person", Score: 0.9, Box: [4]int{12, 12, 62, 62}},
	})
	if len(objs) != 0 {
		t.Errorf("frame 2: should not be confirmed yet, got %d", len(objs))
	}

	// Frame 3: now hits=3, should be confirmed
	objs = tr.Update([]Detection{
		{Label: "person", Score: 0.9, Box: [4]int{14, 14, 64, 64}},
	})
	if len(objs) != 1 {
		t.Errorf("frame 3: should be confirmed, got %d", len(objs))
	}
}

func TestTracker_DeletedTracksReturned(t *testing.T) {
	tr := NewTracker(0, 1) // maxDisappeared=0 -> deleted immediately on miss

	tr.Update([]Detection{
		{Label: "person", Score: 0.9, Box: [4]int{10, 10, 60, 60}},
	})

	// Object disappears
	tr.Update(nil)

	deleted := tr.DeletedTracks()
	if len(deleted) != 1 {
		t.Fatalf("expected 1 deleted track, got %d", len(deleted))
	}
	if deleted[0].Label != "person" {
		t.Errorf("expected label person, got %s", deleted[0].Label)
	}
}

func TestTracker_EmptyUpdate(t *testing.T) {
	tr := NewTracker(3, 1)
	objs := tr.Update(nil)
	if len(objs) != 0 {
		t.Errorf("expected 0 from empty tracker, got %d", len(objs))
	}
}

// ---------------------------------------------------------------------------
// Stationary-track preservation tests.
//
// These verify the tracker's behavior for objects that have come to rest in
// the frame (parked cars, idle pets, packages on a porch). The contract is:
//
//   - Once a track has been matched at high IoU for `stationaryThreshold`
//     consecutive updates *and* its centroid hasn't drifted past
//     `stationaryDriftRatio × max(box dims)` from the streak anchor, it is
//     treated as "stationary".
//
//   - Stationary tracks survive far longer detection gaps
//     (`stationaryMaxDisappeared` instead of `maxDisappeared`) so brief
//     motion-gated YOLO pauses don't churn their TrackIDs.
//
//   - Once a stationary track *moves*, its streak resets and it returns to
//     short decay — so an object that was parked and then leaves doesn't
//     keep a 5-minute zombie alive in the spot it used to occupy.
//
//   - The drift guard ensures slow-moving objects (a person walking
//     ~2 px/frame) aren't falsely promoted just because per-frame IoU stays
//     high.
// ---------------------------------------------------------------------------

// TestTracker_StationaryCarSurvivesMotionGatedGaps reproduces the exact bug
// reported against camera.go: parked car visible, motion gating turns YOLO
// off so the tracker receives long runs of nil detections, and motion later
// re-qualifies. The track MUST keep the same TrackID throughout — otherwise
// the camera fires a fresh event for the same parked car every motion burst.
func TestTracker_StationaryCarSurvivesMotionGatedGaps(t *testing.T) {
	// Short stationary timeout so the test is fast: confirm immediately,
	// promote to stationary after 5 high-IoU matches, then survive up to
	// 200 nil updates (well past the 4-frame non-stationary timeout).
	tr := NewTrackerWithStationary(4, 1, 200, 5, 0.85, 0.3)

	parked := []Detection{{Label: "car", Score: 0.9, Box: [4]int{100, 100, 200, 200}}}

	// Frame 1: first match. Track is confirmed (minHits=1) but not yet
	// stationary — there's no prior box to compare against, so this update
	// MUST NOT count toward the stationary streak.
	objs := tr.Update(parked)
	if len(objs) != 1 {
		t.Fatalf("frame 1: expected 1 confirmed track, got %d", len(objs))
	}
	originalID := objs[0].TrackID

	// Frames 2–6: five more high-IoU matches (1px wobble) — promotes to stationary.
	for i := 0; i < 5; i++ {
		jitter := []Detection{{Label: "car", Score: 0.9, Box: [4]int{100 + i%2, 100, 200 + i%2, 200}}}
		objs = tr.Update(jitter)
		if len(objs) != 1 || objs[0].TrackID != originalID {
			t.Fatalf("frame %d: stationary streak interrupted unexpectedly", i+2)
		}
	}

	// Now run 100 nil updates. With non-stationary maxDisappeared=4 the track
	// would be deleted at frame 5; the stationary timeout of 200 must keep it
	// alive throughout.
	for i := 0; i < 100; i++ {
		objs = tr.Update(nil)
		if len(objs) != 1 {
			t.Fatalf("nil update %d: stationary track lost (got %d objects, expected 1)", i, len(objs))
		}
		if objs[0].TrackID != originalID {
			t.Fatalf("nil update %d: TrackID changed %d → %d", i, originalID, objs[0].TrackID)
		}
	}

	// Motion qualifies again, same parked car. Must reuse the same track —
	// no new TrackID, no new event from the camera's perspective.
	objs = tr.Update(parked)
	if len(objs) != 1 || objs[0].TrackID != originalID {
		t.Fatalf("after long gap: expected TrackID %d, got %v", originalID, objs)
	}

	// And confirm that no resurrection-as-new ever happened: the tracker's
	// internal track count must still be 1.
	if got := len(tr.tracks); got != 1 {
		t.Errorf("expected exactly 1 internal track across the scenario, got %d", got)
	}
}

// TestTracker_SlowWalkerNotPromotedToStationary verifies the drift guard
// with the production stationary parameters (threshold=25, ratio=0.3) — a
// person walking 3 px/frame on a 100×100 box keeps per-frame IoU ~0.94
// (above 0.85, so the streak ticks) but the centroid drifts 30 px every
// 10 frames, which forces a streak reset. stationaryHits never exceeds 9
// between resets, far below the 25 needed for promotion. When the walker
// exits the frame their track must decay at the short timeout, not linger.
func TestTracker_SlowWalkerNotPromotedToStationary(t *testing.T) {
	// Production-shape stationary tuning, but with short timeouts so the
	// test runs fast.
	tr := NewTrackerWithStationary(4, 1, 200, 25, 0.85, 0.3)

	for i := 0; i < 30; i++ {
		d := []Detection{{Label: "person", Score: 0.9, Box: [4]int{100 + 3*i, 100, 200 + 3*i, 200}}}
		objs := tr.Update(d)
		if len(objs) != 1 {
			t.Fatalf("frame %d: expected 1 track, got %d", i, len(objs))
		}
	}

	// Then the person walks out of frame. With stationary preservation the
	// track would survive 200 nil updates. The drift guard must prevent that
	// — track must be deleted at the short timeout.
	for i := 0; i < 5; i++ {
		tr.Update(nil)
	}
	objs := tr.Update(nil)
	if len(objs) != 0 {
		t.Errorf("slow walker should NOT be promoted; expected track deleted at short timeout, got %d alive", len(objs))
	}
}

// TestTracker_StationaryThenMovesDemotesQuickly: a car parks, becomes
// stationary, then drives off. Once it starts moving (low IoU between
// consecutive matches), the stationary flag must clear so when it exits
// the frame the track decays at the short timeout — letting a different
// car parking in the same spot register as a NEW event.
//
// The "drives off" phase uses gradual motion (30 px/frame on a 100 px box →
// per-frame IoU ~0.54). That's below the 0.85 stationarity threshold (so
// the streak resets and the track demotes) but well above 0 (so the
// matcher keeps tracking the same TrackID instead of treating it as a
// teleport and creating a new track).
func TestTracker_StationaryThenMovesDemotesQuickly(t *testing.T) {
	tr := NewTrackerWithStationary(4, 1, 200, 3, 0.85, 0.3)

	// Park: 5 frames at the same spot — promote to stationary.
	parked := []Detection{{Label: "car", Score: 0.9, Box: [4]int{100, 100, 200, 200}}}
	var parkedID int
	for i := 0; i < 5; i++ {
		objs := tr.Update(parked)
		if len(objs) != 1 {
			t.Fatalf("park frame %d: expected 1 track, got %d", i, len(objs))
		}
		parkedID = objs[0].TrackID
	}

	// Drive off: 4 frames of motion that the matcher can still follow.
	for i := 1; i <= 4; i++ {
		moving := []Detection{{Label: "car", Score: 0.9, Box: [4]int{100 + 30*i, 100, 200 + 30*i, 200}}}
		objs := tr.Update(moving)
		if len(objs) != 1 {
			t.Fatalf("move frame %d: expected matcher to follow same track, got %d objects", i, len(objs))
		}
		if objs[0].TrackID != parkedID {
			t.Fatalf("move frame %d: matcher created a new track (%d) instead of following the original (%d)",
				i, objs[0].TrackID, parkedID)
		}
	}

	// Out of frame: 5 nil updates. With stationary still set this would
	// survive (200 timeout); with proper demotion it must be gone.
	for i := 0; i < 5; i++ {
		tr.Update(nil)
	}
	objs := tr.Update(nil)
	if len(objs) != 0 {
		t.Errorf("track that started stationary then moved must demote and decay at short timeout; got %d alive", len(objs))
	}
}

// TestTracker_StationaryDeletedAfterStationaryTimeout: even stationary
// tracks must eventually decay. Otherwise a parked car that's been moved
// (e.g. driven away during a multi-day camera outage) would haunt the
// tracker forever.
func TestTracker_StationaryDeletedAfterStationaryTimeout(t *testing.T) {
	tr := NewTrackerWithStationary(4, 1, 20, 3, 0.85, 0.3)

	parked := []Detection{{Label: "car", Score: 0.9, Box: [4]int{100, 100, 200, 200}}}
	for i := 0; i < 4; i++ {
		tr.Update(parked)
	}

	// Run nil updates well past the stationary timeout.
	for i := 0; i < 25; i++ {
		tr.Update(nil)
	}
	objs := tr.Update(nil)
	if len(objs) != 0 {
		t.Errorf("stationary track must decay after stationaryMaxDisappeared; got %d alive", len(objs))
	}
}

// TestTracker_TwoStationaryObjectsStayDistinctAcrossGaps: two parked cars
// at non-overlapping positions get unique TrackIDs, and after a long
// detection gap each car's track must be matched to its own original ID
// — not swapped.
func TestTracker_TwoStationaryObjectsStayDistinctAcrossGaps(t *testing.T) {
	tr := NewTrackerWithStationary(4, 1, 200, 3, 0.85, 0.3)

	a := [4]int{100, 100, 200, 200}
	b := [4]int{500, 500, 600, 600}
	dets := []Detection{
		{Label: "car", Score: 0.9, Box: a},
		{Label: "car", Score: 0.9, Box: b},
	}

	// Promote both to stationary.
	for i := 0; i < 5; i++ {
		tr.Update(dets)
	}
	objs := tr.Update(dets)
	if len(objs) != 2 {
		t.Fatalf("expected 2 tracks, got %d", len(objs))
	}
	idByPos := map[int]int{}
	for _, o := range objs {
		idByPos[o.Box[0]] = o.TrackID
	}
	idA, idB := idByPos[100], idByPos[500]
	if idA == 0 || idB == 0 || idA == idB {
		t.Fatalf("expected distinct non-zero TrackIDs, got A=%d B=%d", idA, idB)
	}

	// 50 nil updates — both tracks must survive.
	for i := 0; i < 50; i++ {
		tr.Update(nil)
	}

	// Detections return — each at its original spot.
	objs = tr.Update(dets)
	if len(objs) != 2 {
		t.Fatalf("after gap: expected 2 tracks, got %d", len(objs))
	}
	for _, o := range objs {
		if o.Box[0] == 100 && o.TrackID != idA {
			t.Errorf("car A: TrackID changed %d → %d after gap", idA, o.TrackID)
		}
		if o.Box[0] == 500 && o.TrackID != idB {
			t.Errorf("car B: TrackID changed %d → %d after gap", idB, o.TrackID)
		}
	}
}

// TestTracker_FirstFrameDetectionNotStationary: a single-frame appearance
// must NOT count as stationary — there's no prior box to compare against,
// and we mustn't extend the lifetime of a transient flicker.
func TestTracker_FirstFrameDetectionNotStationary(t *testing.T) {
	tr := NewTrackerWithStationary(4, 1, 200, 1, 0.85, 0.3)

	tr.Update([]Detection{{Label: "car", Score: 0.9, Box: [4]int{100, 100, 200, 200}}})

	// Five nil updates: at maxDisappeared=4 the track should be deleted.
	for i := 0; i < 5; i++ {
		tr.Update(nil)
	}
	objs := tr.Update(nil)
	if len(objs) != 0 {
		t.Errorf("single-frame track must not be promoted to stationary on first match; got %d alive", len(objs))
	}
}

// TestTracker_OscillatingBoxStaysStationary: tiny box jitter (e.g. YOLO
// regression noise on a parked car, or a flag flapping in place) keeps
// per-frame IoU high and cumulative drift small — must remain stationary.
func TestTracker_OscillatingBoxStaysStationary(t *testing.T) {
	tr := NewTrackerWithStationary(4, 1, 200, 4, 0.85, 0.3)

	// Oscillate by 1 px in alternating directions for 30 frames.
	for i := 0; i < 30; i++ {
		dx := 0
		if i%2 == 1 {
			dx = 1
		}
		d := []Detection{{Label: "car", Score: 0.9, Box: [4]int{100 + dx, 100, 200 + dx, 200}}}
		objs := tr.Update(d)
		if len(objs) != 1 {
			t.Fatalf("frame %d: lost track with tiny oscillation, got %d", i, len(objs))
		}
	}

	// Then 50 nil updates — must survive (oscillation never accumulated drift).
	for i := 0; i < 50; i++ {
		objs := tr.Update(nil)
		if len(objs) != 1 {
			t.Fatalf("nil update %d: oscillating-stationary track lost", i)
		}
	}
}

// TestTracker_StationaryPromotion_UnderRealisticMotionGating mirrors how
// the camera actually drives the tracker: YOLO is gated behind motion, so
// during quiet stretches `Update` receives nil. A parked car typically
// only gets matched while motion is qualifying near it (someone parking
// it, wind on nearby foliage, a person walking past). The tracker's
// stationary promotion must complete within the *match-active* portion
// of a parked car's lifetime — not require a uniform stream of detections.
//
// Scenario: 12 frames of detections (motion qualifies during arrival),
// then 100 nil updates (motion gates YOLO off), then 12 more frames of
// detections (someone walks past, motion qualifies again). The tracker
// must keep the same TrackID throughout — including across the long nil
// gap — using DEFAULT production parameters (no test-specific tuning).
func TestTracker_StationaryPromotion_UnderRealisticMotionGating(t *testing.T) {
	tr := NewTracker(30, 1) // production defaults; minHits=1 so we don't fight confirmation timing

	parked := []Detection{{Label: "car", Score: 0.9, Box: [4]int{100, 100, 200, 200}}}

	// Phase 1: motion qualifies during arrival — 12 consecutive matches with
	// 1 px wobble. Must accumulate enough stationary hits to cross the
	// default threshold (10).
	var carID int
	for i := 0; i < 12; i++ {
		jitter := []Detection{{Label: "car", Score: 0.9, Box: [4]int{100 + i%2, 100, 200 + i%2, 200}}}
		objs := tr.Update(jitter)
		if len(objs) != 1 {
			t.Fatalf("arrival frame %d: expected 1 track, got %d", i, len(objs))
		}
		carID = objs[0].TrackID
	}

	// Phase 2: motion stops, YOLO is gated off. The default
	// maxDisappeared=30 would delete this track at frame 31. Stationary
	// preservation must keep it alive for far longer — we run 100 nil
	// updates here.
	for i := 0; i < 100; i++ {
		objs := tr.Update(nil)
		if len(objs) != 1 || objs[0].TrackID != carID {
			t.Fatalf("nil update %d: stationary track lost (expected TrackID %d, got %v)",
				i, carID, objs)
		}
	}

	// Phase 3: motion qualifies again (someone walks past). Same parked
	// car must reuse the same TrackID — no new event would fire.
	objs := tr.Update(parked)
	if len(objs) != 1 {
		t.Fatalf("re-detection: expected 1 track, got %d", len(objs))
	}
	if objs[0].TrackID != carID {
		t.Errorf("re-detection: TrackID changed %d → %d (would trigger duplicate event)",
			carID, objs[0].TrackID)
	}
}

// TestTracker_ParkedVanWithRealisticBoxWobbleStaysStationary reproduces the
// production false-positive: a parked van at the front door fires a fresh
// "car" event every few minutes. The van is genuinely stationary (its
// centroid barely moves) but YOLO's bounding box wobbles in SIZE frame to
// frame. Two real production boxes for the same parked van,
// [0,840,720,1440] and [0,900,780,1440], have IoU ~0.837 with centroids only
// ~42 px apart on a 720 px box (well inside the 0.3 x dim drift cap).
//
// A strict per-frame IoU gate (the former 0.85) treats that size wobble as
// motion and resets the promotion streak, so the van never promotes, decays
// at the short non-stationary timeout during the next motion-gated quiet
// window, and re-detects as a NEW TrackID, firing a duplicate event for a
// vehicle that never moved. The fix keeps IoU only as a coarse 0.5
// same-object floor and lets the cumulative drift guard judge stationarity,
// so box-size wobble no longer blocks promotion.
//
// Contract: a stationary object keeps ONE TrackID across a long gap.
func TestTracker_ParkedVanWithRealisticBoxWobbleStaysStationary(t *testing.T) {
	tr := NewTracker(30, 3) // production defaults

	// Two real production boxes for the same parked van; IoU(a,b) ≈ 0.837.
	a := [4]int{0, 840, 720, 1440}
	b := [4]int{0, 900, 780, 1440}

	var vanID int
	// 20 frames of the van while motion qualifies, YOLO alternating between
	// the two box estimates (realistic size wobble).
	for i := 0; i < 20; i++ {
		box := a
		if i%2 == 1 {
			box = b
		}
		objs := tr.Update([]Detection{{Label: "car", Score: 0.9, Box: box}})
		if len(objs) == 1 {
			vanID = objs[0].TrackID
		}
	}
	if vanID == 0 {
		t.Fatal("van never confirmed")
	}

	// Motion-gated quiet window: 60 nil updates (> non-stationary
	// maxDisappeared=30). A promoted-stationary track survives this.
	for i := 0; i < 60; i++ {
		tr.Update(nil)
	}

	// Van re-detected in place. It MUST reuse the same TrackID — otherwise the
	// camera fires a duplicate "car" event for a van that never moved.
	objs := tr.Update([]Detection{{Label: "car", Score: 0.9, Box: a}})
	if len(objs) != 1 {
		t.Fatalf("re-detection: expected 1 track, got %d", len(objs))
	}
	if objs[0].TrackID != vanID {
		t.Errorf("parked van got a NEW TrackID %d after a quiet gap (was %d) — "+
			"realistic box-size wobble defeated stationary promotion, so the "+
			"camera fires a duplicate event for a van that never moved",
			objs[0].TrackID, vanID)
	}
}

// TestTracker_ParkedVanPromotesAcrossMotionGatedGaps mirrors the real
// production cadence: a parked van is only re-detected when nearby motion
// qualifies YOLO, so matched frames arrive in short bursts separated by nil
// gaps. The stationary streak must accumulate across those gaps (nil frames
// preserve it) and promote, even though YOLO's box wobbles in size between
// bursts. Once promoted, the van must survive a long quiet window with one ID.
func TestTracker_ParkedVanPromotesAcrossMotionGatedGaps(t *testing.T) {
	tr := NewTracker(30, 3) // production defaults
	a := [4]int{0, 840, 720, 1440}
	b := [4]int{0, 900, 780, 1440}

	var vanID int
	// 12 matched detections (> threshold 10), each followed by a 5-frame nil
	// gap (well under maxDisappeared=30), with the box alternating to mimic
	// YOLO size wobble.
	for i := 0; i < 12; i++ {
		box := a
		if i%2 == 1 {
			box = b
		}
		objs := tr.Update([]Detection{{Label: "car", Score: 0.9, Box: box}})
		if len(objs) == 1 {
			vanID = objs[0].TrackID
		}
		for j := 0; j < 5; j++ {
			tr.Update(nil)
		}
	}
	if vanID == 0 {
		t.Fatal("van never confirmed")
	}

	// Long quiet window beyond the non-stationary timeout.
	for i := 0; i < 80; i++ {
		tr.Update(nil)
	}
	objs := tr.Update([]Detection{{Label: "car", Score: 0.9, Box: b}})
	if len(objs) != 1 || objs[0].TrackID != vanID {
		t.Errorf("parked van must keep TrackID %d across motion-gated gaps, got %v", vanID, objs)
	}
}

// TestTracker_FastDriveBy_NotPromoted: a car driving past the camera
// (not parking) must NOT be promoted to stationary. At ~25 px/frame the
// per-frame IoU (~0.6) still clears the coarse 0.5 same-object floor, so the
// cumulative drift guard is what rejects it: the centroid leaves the
// 0.3 x box-dim window within a couple of frames and resets the streak every
// time. When the car exits the frame its track must decay at the short
// timeout, not linger as a 5-minute ghost where it last appeared.
func TestTracker_FastDriveBy_NotPromoted(t *testing.T) {
	tr := NewTracker(30, 1)

	// 20 frames moving at 25 px/frame on a 100 px box. IoU between consecutive
	// boxes (~0.6) clears the 0.5 floor so the matcher follows the same track,
	// but cumulative drift keeps the stationary streak from ever building.
	for i := 0; i < 20; i++ {
		d := []Detection{{Label: "car", Score: 0.9, Box: [4]int{100 + 25*i, 100, 200 + 25*i, 200}}}
		tr.Update(d)
	}

	// Car exits frame: nil updates. Must decay at maxDisappeared=30.
	for i := 0; i < 31; i++ {
		tr.Update(nil)
	}
	objs := tr.Update(nil)
	if len(objs) != 0 {
		t.Errorf("drive-by car must not linger as stationary; got %d alive after default decay", len(objs))
	}
}

// TestTracker_WalkerNotPromotedAtProductionDefaults verifies, with the real
// production constructor (IoU floor 0.5, threshold 10, drift ratio 0.3), that
// a person walking at a normal pace is rejected by the cumulative drift guard
// rather than the IoU floor. A walker's per-frame IoU stays well above 0.5,
// so the floor never rejects it; the centroid creeps past 0.3 x box dim from
// the anchor within a few frames and resets the streak before it reaches the
// 10-hit promotion threshold.
func TestTracker_WalkerNotPromotedAtProductionDefaults(t *testing.T) {
	tr := NewTracker(30, 1)

	// 10 px/frame on a 100 px box: per-frame IoU ~0.82 (> 0.5 floor), but the
	// centroid drifts past 0.3 x 100 = 30 px from the anchor within ~4 frames.
	for i := 0; i < 20; i++ {
		d := []Detection{{Label: "person", Score: 0.9, Box: [4]int{100 + 10*i, 100, 200 + 10*i, 200}}}
		tr.Update(d)
	}
	for i := 0; i < 31; i++ { // exceed the non-stationary maxDisappeared
		tr.Update(nil)
	}
	if objs := tr.Update(nil); len(objs) != 0 {
		t.Errorf("a normal-pace walker must not be promoted at production defaults; got %d still alive", len(objs))
	}
}

// TestTracker_ParkedVanThenLeavesDemotesAtProductionDefaults: a van parks
// (promotes under production defaults), then drives off. Once it moves, the
// cumulative drift guard resets the streak and clears the stationary flag, so
// when it leaves the frame the track decays at the short non-stationary
// timeout instead of lingering for ~5 minutes and blocking a different
// vehicle that later parks in the same spot.
func TestTracker_ParkedVanThenLeavesDemotesAtProductionDefaults(t *testing.T) {
	tr := NewTracker(30, 1)
	a := [4]int{0, 840, 720, 1440}
	b := [4]int{0, 900, 780, 1440}

	// Park: 14 wobbling frames promote the van to stationary (threshold 10).
	for i := 0; i < 14; i++ {
		box := a
		if i%2 == 1 {
			box = b
		}
		tr.Update([]Detection{{Label: "car", Score: 0.9, Box: box}})
	}

	// Drive off: each step shifts the centroid past the 0.3 x dim drift cap so
	// the streak resets and the track demotes, while consecutive boxes still
	// overlap enough for the matcher to keep following the same TrackID.
	for i := 1; i <= 3; i++ {
		mv := [4]int{a[0] + 300*i, a[1], a[2] + 300*i, a[3]}
		tr.Update([]Detection{{Label: "car", Score: 0.9, Box: mv}})
	}

	// Out of frame: a demoted track must decay at maxDisappeared=30, not the
	// ~5-minute stationary budget.
	for i := 0; i < 31; i++ {
		tr.Update(nil)
	}
	if objs := tr.Update(nil); len(objs) != 0 {
		t.Errorf("van that parked then drove off must demote and decay at the short timeout; got %d still alive", len(objs))
	}
}

// TestTracker_MovedFlag drives event categorization: a vehicle that only ever
// sits still (a parked car, including one re-detected after a restart) must
// report Moved=false so it can be categorized as a low-priority "detection",
// while an object that travels across the scene reports Moved=true ("alert").
// The signal latches on cumulative displacement from where the track was first
// seen, so it tolerates box wobble but catches real travel.
func TestTracker_MovedFlag(t *testing.T) {
	// Parked car: only box-size jitter (~42px centroid shift on a 720px box).
	parked := NewTracker(30, 1)
	a := [4]int{0, 840, 720, 1440}
	b := [4]int{0, 900, 780, 1440}
	var po TrackedObject
	for i := 0; i < 10; i++ {
		box := a
		if i%2 == 1 {
			box = b
		}
		objs := parked.Update([]Detection{{Label: "car", Score: 0.9, Box: box}})
		if len(objs) == 1 {
			po = objs[0]
		}
	}
	if po.Moved {
		t.Error("a parked car (only jitter) must report Moved=false")
	}

	// Moving car: travels across the frame (60px/frame on a 200px box, boxes
	// still overlap so the matcher follows one track).
	moving := NewTracker(30, 1)
	var mo TrackedObject
	for i := 0; i < 8; i++ {
		box := [4]int{100 + 60*i, 100, 300 + 60*i, 300}
		objs := moving.Update([]Detection{{Label: "car", Score: 0.9, Box: box}})
		if len(objs) == 1 {
			mo = objs[0]
		}
	}
	if !mo.Moved {
		t.Error("a car that traveled across the scene must report Moved=true")
	}
}

// TestTracker_HasStationaryConfirmed drives the motion-gated detect loop's
// re-confirmation decision: the loop periodically re-runs detection on parked
// objects (like Frigate's stationary interval) so their tracks do not age out
// during quiet windows and re-fire as new events. The signal must be true only
// when a confirmed track is currently classified as stationary.
func TestTracker_HasStationaryConfirmed(t *testing.T) {
	empty := NewTracker(30, 1)
	if empty.HasStationaryConfirmed() {
		t.Fatal("empty tracker must report no stationary confirmed tracks")
	}

	// A moving (confirmed) object must NOT count as stationary.
	moving := NewTracker(30, 1)
	for i := 0; i < 6; i++ {
		moving.Update([]Detection{{Label: "person", Score: 0.9, Box: [4]int{100 + 20*i, 100, 200 + 20*i, 200}}})
	}
	if moving.HasStationaryConfirmed() {
		t.Error("a moving confirmed object must not be reported as stationary")
	}

	// A parked van promotes to stationary and must be reported.
	parked := NewTracker(30, 1)
	a := [4]int{0, 840, 720, 1440}
	b := [4]int{0, 900, 780, 1440}
	for i := 0; i < 14; i++ {
		box := a
		if i%2 == 1 {
			box = b
		}
		parked.Update([]Detection{{Label: "car", Score: 0.9, Box: box}})
	}
	if !parked.HasStationaryConfirmed() {
		t.Error("a parked, promoted vehicle must be reported as stationary confirmed")
	}
}

// TestTracker_NewTracker_HasReasonableStationaryDefaults: the default
// constructor (used by camera.go) must enable stationary preservation
// with sensible values — otherwise the production fix is a no-op.
func TestTracker_NewTracker_HasReasonableStationaryDefaults(t *testing.T) {
	tr := NewTracker(30, 3)
	if tr.stationaryMaxDisappeared <= tr.maxDisappeared {
		t.Errorf("stationaryMaxDisappeared (%d) must be > maxDisappeared (%d)",
			tr.stationaryMaxDisappeared, tr.maxDisappeared)
	}
	if tr.stationaryThreshold <= 0 {
		t.Errorf("stationaryThreshold must be > 0, got %d", tr.stationaryThreshold)
	}
	if tr.stationaryIoU <= 0 || tr.stationaryIoU >= 1 {
		t.Errorf("stationaryIoU must be in (0, 1), got %f", tr.stationaryIoU)
	}
	if tr.stationaryDriftRatio <= 0 {
		t.Errorf("stationaryDriftRatio must be > 0, got %f", tr.stationaryDriftRatio)
	}
}

// TestTracker_StaleTrackReusedWhenUpdateSkipped documents the tracker contract
// that drives the camera fix: decay only happens during Update calls. If a
// caller stops invoking Update, tracks freeze in place, and IoU matching will
// later reassign their TrackIDs to new detections at overlapping positions.
//
// This is exactly what bit camera.go: tracker.Update was gated behind motion,
// so during quiet periods tracks didn't age, the same TrackID came back for
// new detections, and the confirmedTracks lookup suppressed the new event.
func TestTracker_StaleTrackReusedWhenUpdateSkipped(t *testing.T) {
	tr := NewTracker(2, 1) // confirm immediately, decay after 2 misses

	objs := tr.Update([]Detection{
		{Label: "car", Score: 0.9, Box: [4]int{10, 10, 60, 60}},
	})
	if len(objs) != 1 {
		t.Fatalf("setup: expected 1 confirmed, got %d", len(objs))
	}
	originalID := objs[0].TrackID

	// Simulate the bug: skip every Update call during a long quiet period.
	// (No tr.Update calls at all here.)

	// Motion resumes — new detection at the SAME position. Because the track
	// was never aged, IoU matching reuses it instead of creating a new one.
	objs = tr.Update([]Detection{
		{Label: "car", Score: 0.95, Box: [4]int{10, 10, 60, 60}},
	})
	if len(objs) != 1 {
		t.Fatalf("after skip: expected 1 confirmed, got %d", len(objs))
	}
	if objs[0].TrackID != originalID {
		t.Errorf("expected stale TrackID %d to be reused (it wasn't, got %d) — "+
			"this would mean the tracker decayed without Update calls, breaking "+
			"the contract that the camera-side fix relies on", originalID, objs[0].TrackID)
	}

	// And by contrast: when the caller HAD called Update with empty frames,
	// the track decays after maxDisappeared misses, so the next detection
	// gets a fresh TrackID.
	tr2 := NewTracker(2, 1)
	objs = tr2.Update([]Detection{
		{Label: "car", Score: 0.9, Box: [4]int{10, 10, 60, 60}},
	})
	originalID = objs[0].TrackID
	for i := 0; i < 3; i++ { // 3 > maxDisappeared=2
		tr2.Update(nil)
	}
	objs = tr2.Update([]Detection{
		{Label: "car", Score: 0.95, Box: [4]int{10, 10, 60, 60}},
	})
	if len(objs) != 1 {
		t.Fatalf("after decay: expected 1 confirmed, got %d", len(objs))
	}
	if objs[0].TrackID == originalID {
		t.Errorf("after maxDisappeared empty Update calls the track should be "+
			"deleted and a new TrackID issued, but got the original ID %d", originalID)
	}
}

// A confirmed track keeps being returned while it coasts through frames with no
// detection, so callers cannot tell "I see it" from "I still believe in it".
// Presence tracking needs that distinction: a zone must reflect what was
// observed, not what is merely remembered.
func TestTracker_DetectedReportsWhetherMatchedThisFrame(t *testing.T) {
	tr := NewTracker(5, 1) // maxDisappeared=5, confirm after 1 hit
	det := Detection{Label: "car", Score: 0.9, Box: [4]int{10, 10, 50, 50}}

	objs := tr.Update([]Detection{det})
	if len(objs) != 1 {
		t.Fatalf("expected 1 confirmed object, got %d", len(objs))
	}
	if !objs[0].Detected {
		t.Errorf("object matched by a detection this frame: Detected = false, want true")
	}

	// No detection this frame: the track coasts, but nothing was observed.
	objs = tr.Update(nil)
	if len(objs) != 1 {
		t.Fatalf("expected the track to still coast, got %d objects", len(objs))
	}
	if objs[0].Detected {
		t.Errorf("object coasting with no detection: Detected = true, want false")
	}

	// A fresh detection re-observes it.
	objs = tr.Update([]Detection{det})
	if len(objs) != 1 || !objs[0].Detected {
		t.Errorf("object re-matched by a detection: Detected = false, want true")
	}
}
