package recording

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/media"
	"github.com/rvben/vedetta/internal/storage"
)

// newClipRecompressor builds a recompressor that prefers clips, so processOne
// picks the seeded clip rather than a segment.
func newClipRecompressor(t *testing.T, db *storage.DB) *Recompressor {
	t.Helper()
	cfg := config.TieredStorageConfig{
		Enabled: true, AfterDays: 1, Schedule: "00:00-23:59",
		Interval: time.Second, Priority: "largest", TargetWidth: 640, TargetHeight: 360,
	}
	return NewRecompressor(cfg, []config.CameraConfig{{Name: "cam1"}}, db, &sync.Mutex{})
}

// TestPermanentFailureKindsAreExactlyTheNonRetryableOnes keeps the list handed
// to storage tied to the retryable() decision. Storage cannot check the policy
// it is given, so a kind that drifts out of this list silently returns to being
// retried forever, which is the bug this whole change removes.
func TestPermanentFailureKindsAreExactlyTheNonRetryableOnes(t *testing.T) {
	got := permanentFailureKinds()
	if len(got) == 0 {
		t.Fatal("no permanent kinds: every failure would be retried at every restart")
	}

	for _, k := range got {
		if transcodeFailureKind(k).retryable() {
			t.Errorf("%q is in the permanent list but retryable() says it should be retried", k)
		}
	}

	// Every kind the parent can record must be accounted for in one direction
	// or the other.
	for _, k := range allTranscodeFailureKinds {
		inList := slices.Contains(got, string(k))
		if inList == k.retryable() {
			t.Errorf("kind %q: permanent list membership=%v but retryable=%v", k, inList, k.retryable())
		}
	}
}

// TestRecompressor_PermanentFailureSurvivesTheStartupReset is the production
// bug end to end. Eighteen segments whose video no decoder accepts were retried
// exactly 25 times each: three attempts, then a restart cleared the counter,
// then three more. The counter reset is still right for a transient cause, so
// the fix has to distinguish them rather than remove it.
func TestRecompressor_PermanentFailureSurvivesTheStartupReset(t *testing.T) {
	r, db := newTestRecompressor(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "undecodable.mp4")
	if err := os.WriteFile(path, []byte("recording"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := saveSegmentForTest(t, db, path)

	r.transcodeFn = func(string, int, int) (media.TranscodeResult, error) {
		return media.TranscodeResult{}, &transcodeWorkerError{
			kind: transcodeFailureSourceUndecodable,
			err:  errors.New("no keyframe decoded in any of 6 fragments"),
		}
	}

	for i := range 3 {
		if !r.processOne() {
			t.Fatalf("attempt %d: processOne returned false, want a handled failure", i+1)
		}
	}

	cutoff := time.Now().Add(-time.Hour)
	if segs, _ := db.GetSegmentsForRecompression("cam1", cutoff); len(segs) != 0 {
		t.Fatalf("segment still eligible after 3 failures: %+v", segs)
	}

	// The restart. Everything a new process does to the failure counters.
	reset, err := db.ResetStuckRecompressFailures(permanentFailureKinds())
	if err != nil {
		t.Fatalf("ResetStuckRecompressFailures: %v", err)
	}
	if reset != 0 {
		t.Errorf("reset %d rows, want 0: this file cannot be recompressed by any attempt", reset)
	}

	segs, err := db.GetSegmentsForRecompression("cam1", cutoff)
	if err != nil {
		t.Fatalf("GetSegmentsForRecompression: %v", err)
	}
	for _, s := range segs {
		if s.ID == id {
			t.Error("undecodable segment is queued again after a restart")
		}
	}
}

// TestRecompressor_TransientFailureIsRetriedAfterRestart is the other half, and
// the reason the reset exists. A host that was missing OpenH264 must not retire
// every segment it touched while broken.
func TestRecompressor_TransientFailureIsRetriedAfterRestart(t *testing.T) {
	r, db := newTestRecompressor(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "fine.mp4")
	if err := os.WriteFile(path, []byte("recording"), 0o600); err != nil {
		t.Fatal(err)
	}
	saveSegmentForTest(t, db, path)

	r.transcodeFn = func(string, int, int) (media.TranscodeResult, error) {
		return media.TranscodeResult{}, &transcodeWorkerError{
			kind: transcodeFailureCodecUnavailable,
			err:  errors.New("OpenH264 not available"),
		}
	}

	for i := range 3 {
		if !r.processOne() {
			t.Fatalf("attempt %d: processOne returned false, want a handled failure", i+1)
		}
	}

	reset, err := db.ResetStuckRecompressFailures(permanentFailureKinds())
	if err != nil {
		t.Fatalf("ResetStuckRecompressFailures: %v", err)
	}
	if reset != 1 {
		t.Errorf("reset %d rows, want 1: a missing codec is fixed by installing one", reset)
	}
	if segs, _ := db.GetSegmentsForRecompression("cam1", time.Now().Add(-time.Hour)); len(segs) != 1 {
		t.Errorf("%d segments eligible after the reset, want 1", len(segs))
	}
}

// TestRecompressor_UnclassifiedFailureIsRetriedAfterRestart covers the failures
// that carry no worker error at all, which is every failure raised inside the
// parent. They must keep the behaviour they have today.
func TestRecompressor_UnclassifiedFailureIsRetriedAfterRestart(t *testing.T) {
	r, db := newTestRecompressor(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "mystery.mp4")
	if err := os.WriteFile(path, []byte("recording"), 0o600); err != nil {
		t.Fatal(err)
	}
	saveSegmentForTest(t, db, path)

	r.transcodeFn = func(string, int, int) (media.TranscodeResult, error) {
		return media.TranscodeResult{}, errAlwaysFail
	}

	for i := range 3 {
		if !r.processOne() {
			t.Fatalf("attempt %d: processOne returned false, want a handled failure", i+1)
		}
	}

	reset, err := db.ResetStuckRecompressFailures(permanentFailureKinds())
	if err != nil {
		t.Fatalf("ResetStuckRecompressFailures: %v", err)
	}
	if reset != 1 {
		t.Errorf("reset %d rows, want 1: an unidentified cause is not evidence the file is beyond help", reset)
	}
}

// TestRecompressor_ClipPermanentFailureSurvivesTheStartupReset repeats the rule
// for clips. Clips and segments carry separate counters in separate tables, so
// fixing one leaves the other looping.
func TestRecompressor_ClipPermanentFailureSurvivesTheStartupReset(t *testing.T) {
	_, db := newTestRecompressor(t)
	dir := t.TempDir()
	seedClip(t, db, dir, "clipU", "cam1", 47*time.Hour, 500)

	r := newClipRecompressor(t, db)
	r.transcodeFn = func(string, int, int) (media.TranscodeResult, error) {
		return media.TranscodeResult{}, &transcodeWorkerError{
			kind: transcodeFailureSourceNotFragmented,
			err:  errors.New("source has no moof fragments"),
		}
	}

	for i := range 3 {
		if !r.processOne() {
			t.Fatalf("attempt %d: processOne returned false, want a handled failure", i+1)
		}
	}

	reset, err := db.ResetStuckClipRecompressFailures(permanentFailureKinds())
	if err != nil {
		t.Fatalf("ResetStuckClipRecompressFailures: %v", err)
	}
	if reset != 0 {
		t.Errorf("reset %d rows, want 0: a file with no fragments gains none by waiting", reset)
	}
	if clips, _ := db.GetClipsForRecompression("cam1", time.Now().Add(-time.Hour)); len(clips) != 0 {
		t.Errorf("%d clips eligible after the reset, want 0", len(clips))
	}
}
