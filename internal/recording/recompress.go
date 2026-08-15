package recording

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/media"
	"github.com/rvben/vedetta/internal/storage"
)

// segmentCandidateBatch bounds how many largest-priority segment candidates
// processOne fetches per camera so it can skip HLS-in-use files and still
// pick the next-largest eligible segment.
const segmentCandidateBatch = 16

// Recompressor runs scheduled overnight transcoding of old segments and event clips.
type Recompressor struct {
	cfg     config.TieredStorageConfig
	cameras []config.CameraConfig
	db      *storage.DB

	// transcodeFn performs the actual transcoding. Defaults to
	// outOfProcessTranscode, which runs the OpenH264 encode path in an isolated
	// child process so a heap-corruption crash there cannot take down the NVR.
	transcodeFn func(path string, targetW, targetH int) (media.TranscodeResult, error)

	// inUseFn reports whether a segment file is currently HLS-served and must
	// not be recompressed. Defaults to media.HLSPathInUse.
	inUseFn func(path string) bool

	// statFn reports a path's existence during revalidation. Defaults to
	// os.Stat; injectable so tests can exercise the missing-file and transient
	// stat-error branches deterministically.
	statFn func(path string) (os.FileInfo, error)

	// lock is the shared segment-operation mutex from the owning Recorder.
	// processOne holds it across the transcode+DB-update pair so that
	// retention cleanup and emergency delete cannot race with an in-flight
	// recompression.
	lock *sync.Mutex

	isRunning            atomic.Bool
	mu                   sync.Mutex
	lastRun              time.Time
	segmentsRecompressed int64
	clipsRecompressed    int64
	bytesReclaimed       int64
	transcodeFailures    int64
	workerCrashes        int64
	workerTimeouts       int64
}

// NewRecompressor creates a Recompressor with the given config and camera list.
// lock is the shared segmentOpMu from the owning Recorder and must not be nil.
func NewRecompressor(cfg config.TieredStorageConfig, cameras []config.CameraConfig, db *storage.DB, lock *sync.Mutex) *Recompressor {
	return &Recompressor{cfg: cfg, cameras: cameras, db: db, lock: lock, transcodeFn: outOfProcessTranscode, inUseFn: media.HLSPathInUse, statFn: os.Stat}
}

// RecompressorStats holds runtime counters for the recompression job.
type RecompressorStats struct {
	LastRun              time.Time
	SegmentsRecompressed int64
	ClipsRecompressed    int64
	BytesReclaimed       int64
	TranscodeFailures    int64
	WorkerCrashes        int64
	WorkerTimeouts       int64
	IsRunning            bool
}

// Stats returns a snapshot of the recompressor's runtime counters.
func (r *Recompressor) Stats() RecompressorStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return RecompressorStats{
		LastRun:              r.lastRun,
		SegmentsRecompressed: r.segmentsRecompressed,
		ClipsRecompressed:    r.clipsRecompressed,
		BytesReclaimed:       r.bytesReclaimed,
		TranscodeFailures:    r.transcodeFailures,
		WorkerCrashes:        r.workerCrashes,
		WorkerTimeouts:       r.workerTimeouts,
		IsRunning:            r.isRunning.Load(),
	}
}

// Start runs the recompression job in a background goroutine until ctx is cancelled.
// Does nothing if tiered storage is disabled.
func (r *Recompressor) Start(ctx context.Context) {
	if !r.cfg.Enabled {
		return
	}
	// Give segments stuck at the 3-failure cap another chance. A common
	// reason for stuck failures is a transiently missing codec (e.g.
	// OpenH264 wasn't installed yet during earlier recompression passes);
	// once the environment is restored, we should retry rather than
	// permanently excluding every segment that was attempted during the
	// broken period.
	if reset, err := r.db.ResetStuckRecompressFailures(); err != nil {
		slog.Warn("recompression: failed to reset stuck failures", "error", err)
	} else if reset > 0 {
		slog.Info("recompression: reset stuck failure counters", "segments", reset)
	}

	if reset, err := r.db.ResetStuckClipRecompressFailures(); err != nil {
		slog.Warn("recompression: failed to reset stuck clip failures", "error", err)
	} else if reset > 0 {
		slog.Info("recompression: reset stuck clip failure counters", "clips", reset)
	}

	// Backfill missing clip sizes once, in bounded batches under the shared
	// lock, before the worker starts. The largest-first query orders by
	// clip_size_bytes, so legacy rows at 0 would otherwise lose to any
	// positive-size candidate until nothing else was eligible.
	r.backfillClipSizes()

	go func() {
		interval := r.cfg.Interval
		if interval <= 0 {
			interval = 30 * time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				inWindow, err := config.InScheduleWindow(r.cfg.Schedule, time.Now())
				if err != nil {
					slog.Warn("recompression: invalid schedule", "schedule", r.cfg.Schedule, "error", err)
					continue
				}
				if !inWindow {
					continue
				}
				r.processOne()
			}
		}
	}()
}

// eligibleCameras returns the effective tiered storage config for each camera
// that has tiered storage enabled, keyed by camera name.
func (r *Recompressor) eligibleCameras() map[string]config.TieredStorageConfig {
	result := make(map[string]config.TieredStorageConfig)
	for _, cam := range r.cameras {
		eff := cam.EffectiveTieredStorage(r.cfg)
		if eff.Enabled {
			result[cam.Name] = eff
		}
	}
	return result
}

// RunNow runs a full recompression pass outside the schedule window.
// It returns immediately if a pass is already running.
// The pass continues until no more eligible segments remain.
func (r *Recompressor) RunNow(ctx context.Context) {
	if !r.isRunning.CompareAndSwap(false, true) {
		return
	}
	defer r.isRunning.Store(false)

	slog.Info("recompression: manual pass started")
	processed := 0
	for {
		select {
		case <-ctx.Done():
			slog.Info("recompression: manual pass cancelled", "processed", processed)
			return
		default:
		}
		if !r.processOne() {
			break
		}
		processed++
	}
	slog.Info("recompression: manual pass completed", "processed", processed)
}

// safeTranscode invokes the configured transcoder with panic recovery. The
// production transcoder is process-isolated; recovery still protects tests and
// alternative implementations supplied through transcodeFn.
func (r *Recompressor) safeTranscode(path string) (result media.TranscodeResult, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic during transcode: %v", p)
		}
	}()
	return r.transcodeFn(path, r.cfg.TargetWidth, r.cfg.TargetHeight)
}

// backfillClipSizes runs the one-time clip size backfill in bounded batches.
// Each batch is taken under the shared lock so it cannot race manual delete /
// re-extract / cleanup into a stale size.
func (r *Recompressor) backfillClipSizes() {
	const batch = 200
	total := 0
	for {
		r.lock.Lock()
		examined, err := r.db.BackfillClipSizes(batch)
		r.lock.Unlock()
		if err != nil {
			slog.Warn("recompression: clip size backfill failed", "error", err)
			return
		}
		total += examined
		if examined < batch {
			break
		}
	}
	if total > 0 {
		slog.Info("recompression: backfilled clip sizes", "examined", total)
	}
}

type recompressKind int

const (
	kindSegment recompressKind = iota
	kindClip
)

// String renders the kind for structured logs.
func (k recompressKind) String() string {
	switch k {
	case kindSegment:
		return "segment"
	case kindClip:
		return "clip"
	default:
		return "unknown"
	}
}

// recompressTarget is a data-only candidate for recompression. Segment IDs are
// int64 and clip (event) IDs are string, so the target carries both rather than
// a lossy stringly-typed id; behavior is selected by kind.
type recompressTarget struct {
	kind      recompressKind
	segID     int64  // valid when kind == kindSegment
	eventID   string // valid when kind == kindClip
	camera    string
	path      string
	sizeBytes int64
	endTime   time.Time
	failures  int
}

// processOne picks the single best eligible artifact (segment or clip) across
// all enabled cameras, ranked by the configured priority, and transcodes it.
// Returns true if an artifact was processed (or deliberately skipped after
// acquiring the lock), false if nothing was eligible.
func (r *Recompressor) processOne() bool {
	now := time.Now()

	priority := r.cfg.Priority
	if priority == "" {
		priority = "largest"
	}

	var best *recompressTarget
	consider := func(c recompressTarget) {
		if best == nil {
			t := c
			best = &t
			return
		}
		if priority == "largest" {
			if c.sizeBytes > best.sizeBytes {
				t := c
				best = &t
			}
		} else if c.endTime.Before(best.endTime) {
			t := c
			best = &t
		}
	}

	for camName, eff := range r.eligibleCameras() {
		cutoff := now.Add(-time.Duration(eff.AfterDays) * 24 * time.Hour)

		// Best segment candidate. Skip in-use (HLS-served) segments during
		// selection so an in-use segment never becomes the best target and
		// stalls the whole cycle.
		var segs []storage.SegmentRecord
		var err error
		if priority == "largest" {
			segs, err = r.db.GetRecompressionCandidatesBySize(camName, cutoff, segmentCandidateBatch)
		} else {
			segs, err = r.db.GetSegmentsForRecompression(camName, cutoff)
		}
		if err != nil {
			slog.Warn("recompression: segment query failed", "camera", camName, "error", err)
		} else {
			for _, s := range segs {
				if r.inUseFn(s.Path) {
					continue
				}
				consider(recompressTarget{
					kind: kindSegment, segID: s.ID, camera: s.Camera,
					path: s.Path, sizeBytes: s.SizeBytes, endTime: s.EndTime,
					failures: s.RecompressFailures,
				})
				break // segs is priority-ordered; first non-in-use wins
			}
		}

		// Best clip candidate. Clips are not HLS-served, so no in-use check.
		var clips []storage.ClipRecord
		if priority == "largest" {
			clips, err = r.db.GetClipRecompressionCandidatesBySize(camName, cutoff, 1)
		} else {
			clips, err = r.db.GetClipsForRecompression(camName, cutoff)
		}
		if err != nil {
			slog.Warn("recompression: clip query failed", "camera", camName, "error", err)
		} else if len(clips) > 0 {
			c := clips[0]
			consider(recompressTarget{
				kind: kindClip, eventID: c.EventID, camera: c.Camera,
				path: c.ClipPath, sizeBytes: c.ClipSizeBytes, endTime: c.EndTime,
				failures: c.RecompressFailures,
			})
		}
	}

	if best == nil {
		return false
	}

	// Acquire the shared segment-operation lock. TryLock is intentional: the
	// recompressor is a background optimization and it is preferable to skip a
	// cycle than to block a user-initiated delete or urgent cleanup.
	if !r.lock.TryLock() {
		slog.Debug("recompression: skipping (another operation in flight)", "path", best.path)
		return false
	}
	defer r.lock.Unlock()

	// Revalidate under the lock: candidate selection happened before we held
	// the lock, so cleanup / delete / re-extract may have changed DB or
	// filesystem state.
	if !r.revalidate(best) {
		return true
	}

	// Clip retention deletes by file mtime, so capture the clip's mtime before
	// transcoding and restore it afterward. Recompression rewrites the file
	// (temp + rename), which would otherwise reset the retention clock and keep
	// the clip far longer than event_retain_days.
	var clipModTime time.Time
	if best.kind == kindClip {
		if fi, statErr := os.Stat(best.path); statErr == nil {
			clipModTime = fi.ModTime()
		}
	}

	start := time.Now()
	result, err := r.safeTranscode(best.path)
	if err != nil {
		failureType := "transcode_error"
		var workerErr *transcodeWorkerError
		if errors.As(err, &workerErr) {
			failureType = string(workerErr.kind)
		}
		r.recordTranscodeFailure(err)
		slog.Warn("recompression: failed",
			"kind", best.kind, "camera", best.camera, "path", best.path,
			"error", err, "failure_type", failureType, "retry", best.failures+1)
		r.incrementFailure(best)
		return true
	}

	newSize := result.NewSize
	if result.Skipped {
		newSize = best.sizeBytes
	}
	if err := r.markRecompressed(best, newSize); err != nil {
		slog.Error("recompression: failed to mark recompressed", "kind", best.kind, "path", best.path, "error", err)
		return true
	}
	if best.kind == kindClip && !result.Skipped && !clipModTime.IsZero() {
		if cherr := os.Chtimes(best.path, clipModTime, clipModTime); cherr != nil {
			slog.Warn("recompression: failed to preserve clip mtime", "id", best.eventID, "path", best.path, "error", cherr)
		}
	}
	if result.Skipped {
		slog.Debug("recompression: skipped (already small enough)", "path", best.path)
		return true
	}

	saved := result.OriginalSize - result.NewSize
	r.mu.Lock()
	r.lastRun = time.Now()
	switch best.kind {
	case kindSegment:
		r.segmentsRecompressed++
	case kindClip:
		r.clipsRecompressed++
	}
	r.bytesReclaimed += saved
	r.mu.Unlock()

	slog.Info("recompression: completed",
		"kind", best.kind, "camera", best.camera, "path", best.path,
		"original_mb", result.OriginalSize/(1024*1024),
		"new_mb", result.NewSize/(1024*1024),
		"saved_mb", saved/(1024*1024),
		"duration", time.Since(start).Round(time.Second))
	return true
}

func (r *Recompressor) recordTranscodeFailure(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.transcodeFailures++
	var workerErr *transcodeWorkerError
	if !errors.As(err, &workerErr) {
		return
	}
	switch workerErr.kind {
	case transcodeFailureCrash:
		r.workerCrashes++
	case transcodeFailureTimeout:
		r.workerTimeouts++
	}
}

// revalidate re-reads the target's row under the lock and returns true only if
// the artifact is still eligible to transcode.
func (r *Recompressor) revalidate(t *recompressTarget) bool {
	switch t.kind {
	case kindSegment:
		seg, err := r.db.GetSegmentByID(t.segID)
		if err != nil {
			slog.Warn("recompression: revalidate segment failed", "id", t.segID, "error", err)
			return false
		}
		if seg == nil || seg.Recompressed {
			return false
		}
		// Missing-file check: retention or a manual delete may have removed the
		// file after selection, leaving an orphaned row. Without this the segment
		// is reselected every cycle, fails transcode with ENOENT, and - because
		// the failure cap is reset on every startup - retries forever. Delete the
		// dead row so it stops being a candidate. Act only on a definitive
		// does-not-exist error, so a transient mount / I/O blip never destroys
		// metadata whose file is still there.
		if _, statErr := r.statFn(seg.Path); statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				if derr := r.db.DeleteSegmentByID(seg.ID); derr != nil {
					slog.Warn("recompression: failed to delete orphaned segment row", "id", seg.ID, "path", seg.Path, "error", derr)
				} else {
					slog.Info("recompression: deleted orphaned segment row (file missing)", "id", seg.ID, "path", seg.Path)
				}
			} else {
				slog.Warn("recompression: segment stat failed, skipping", "id", seg.ID, "path", seg.Path, "error", statErr)
			}
			return false
		}
		return true
	case kindClip:
		st, ok, err := r.db.GetClipRecompressState(t.eventID)
		if err != nil {
			slog.Warn("recompression: revalidate clip failed", "id", t.eventID, "error", err)
			return false
		}
		if !ok || !st.ClipAvailable || st.Recompressed || st.ClipPath != t.path {
			return false
		}
		// Missing-file check: cleanup may have deleted the file after
		// selection, or a legacy row may carry a stale clip_available. Flip
		// availability off and skip rather than waste a transcode or record a
		// spurious failure. Relevant under priority=oldest, which ignores size
		// and could otherwise pick a size-0 missing clip.
		if _, err := os.Stat(t.path); err != nil {
			if uerr := r.db.UpdateEventClipAvailability(t.eventID, false); uerr != nil {
				slog.Warn("recompression: clear stale clip availability", "id", t.eventID, "error", uerr)
			}
			return false
		}
		return true
	}
	return false
}

func (r *Recompressor) markRecompressed(t *recompressTarget, newSize int64) error {
	switch t.kind {
	case kindSegment:
		return r.db.MarkSegmentRecompressed(t.segID, newSize)
	case kindClip:
		return r.db.MarkClipRecompressed(t.eventID, newSize)
	}
	return nil
}

func (r *Recompressor) incrementFailure(t *recompressTarget) {
	var err error
	switch t.kind {
	case kindSegment:
		err = r.db.IncrementSegmentRecompressFailures(t.segID)
	case kindClip:
		err = r.db.IncrementClipRecompressFailures(t.eventID)
	}
	if err != nil {
		slog.Error("recompression: failed to increment failure count", "kind", t.kind, "error", err)
	}
}
