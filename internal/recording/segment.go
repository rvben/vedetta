package recording

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/media"
	"github.com/rvben/vedetta/internal/rtsp"
	"github.com/rvben/vedetta/internal/safepath"
	"github.com/rvben/vedetta/internal/storage"
)

// Segment represents a recorded video file covering a time range.
type Segment struct {
	Path      string
	Camera    string
	StartTime time.Time
	EndTime   time.Time
}

// SegmentRecorder continuously records RTSP streams into fixed-length segments
// using the native Go media pipeline (no ffmpeg).
type SegmentRecorder struct {
	config    config.RecordingConfig
	baseDir   string
	db        *storage.DB
	hub       *rtsp.Hub
	disk      *media.DiskSpace
	wg        sync.WaitGroup
	mu        sync.Mutex
	consumers []*media.RecordingConsumer
	sessions  map[string]*recordingSession
}

// recordingSession is the single recording of one camera. Overlapping callers
// (two events on the same camera, or an event during continuous recording) each
// hold a lease on the same session instead of starting a second recorder: two
// recorders on one camera derive the same second-resolution filename, truncate
// each other's file, and collapse into one DB row. The session runs until the
// last lease is released, so a second event extends recording rather than
// restarting it. Every field is guarded by SegmentRecorder.mu.
type recordingSession struct {
	cancel  context.CancelFunc
	closed  chan struct{} // closed when the last lease is released
	done    chan struct{} // closed when the record loop has exited
	refs    int
	stopped bool
}

func NewSegmentRecorder(cfg config.RecordingConfig, db *storage.DB, hub *rtsp.Hub) *SegmentRecorder {
	baseDir := cfg.Path
	if abs, err := filepath.Abs(baseDir); err == nil {
		baseDir = abs
	}
	disk := media.NewDiskSpace(baseDir)
	disk.SetThreshold(uint64(cfg.MinDiskFreeBytes()), db.GetLargestSegmentSizeSince)
	return &SegmentRecorder{
		config:   cfg,
		baseDir:  baseDir,
		db:       db,
		hub:      hub,
		disk:     disk,
		sessions: make(map[string]*recordingSession),
	}
}

// StartRecording begins continuous segment recording for a camera
// by creating a RecordingConsumer that receives RTP packets from the Hub.
// Calling it again for a camera that is already recording takes an additional
// lease on the running session: recording then continues until every caller's
// context has ended, and no second writer is attached to the same camera.
func (sr *SegmentRecorder) StartRecording(ctx context.Context, cameraName, rtspURL string) {
	segDir, err := safepath.Join(sr.baseDir, cameraName, "segments")
	if err != nil {
		// Invalid path is a permanent config error, not a transient I/O
		// condition: nothing to retry.
		slog.Error("invalid segment directory", "camera", cameraName, "error", err)
		return
	}

	sr.mu.Lock()
	if sr.sessions == nil {
		sr.sessions = make(map[string]*recordingSession)
	}
	if sess, ok := sr.sessions[cameraName]; ok && !sess.stopped {
		sess.refs++
		// refs is read here rather than in the log call: releaseLease can
		// decrement it the moment the mutex is dropped, and a logging argument
		// is an ordinary read that races with that decrement whether or not the
		// level is enabled.
		holders := sess.refs
		sr.mu.Unlock()
		slog.Debug("recording session extended", "camera", cameraName, "holders", holders)
		go sr.holdLease(ctx, sess)
		return
	}

	// A session that is already stopping cannot be revived (its record loop is
	// finalizing the current file), so the new one waits for it to finish
	// rather than briefly running two writers on the same camera.
	prev := sr.sessions[cameraName]

	// The session outlives any single caller, so it must not inherit one
	// caller's cancellation; leases end it instead.
	sessCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	sess := &recordingSession{
		cancel: cancel,
		closed: make(chan struct{}),
		done:   make(chan struct{}),
		refs:   1,
	}
	sr.sessions[cameraName] = sess
	sr.wg.Add(1)
	sr.mu.Unlock()

	go func() {
		defer sr.wg.Done()
		defer close(sess.done)
		defer func() {
			sr.mu.Lock()
			if cur, ok := sr.sessions[cameraName]; ok && cur == sess {
				delete(sr.sessions, cameraName)
			}
			sr.mu.Unlock()
		}()
		if prev != nil {
			select {
			case <-prev.done:
			case <-sessCtx.Done():
				return
			}
		}
		sr.recordLoop(sessCtx, cameraName, rtspURL, segDir)
	}()

	go sr.holdLease(ctx, sess)
}

// holdLease releases the caller's lease on the session once the caller's
// context ends (or the session has already stopped for another reason).
func (sr *SegmentRecorder) holdLease(ctx context.Context, sess *recordingSession) {
	select {
	case <-ctx.Done():
	case <-sess.closed:
	}
	sr.releaseLease(sess)
}

// releaseLease drops one lease and stops the session when the last one goes.
func (sr *SegmentRecorder) releaseLease(sess *recordingSession) {
	sr.mu.Lock()
	if sess.refs > 0 {
		sess.refs--
	}
	stop := sess.refs == 0 && !sess.stopped
	if stop {
		sess.stopped = true
	}
	sr.mu.Unlock()
	if stop {
		close(sess.closed)
		sess.cancel()
	}
}

// StopRecording stops recording for a single camera, regardless of how many
// callers still hold a lease on it.
func (sr *SegmentRecorder) StopRecording(cameraName string) {
	sr.mu.Lock()
	sess, ok := sr.sessions[cameraName]
	stop := ok && !sess.stopped
	if stop {
		sess.stopped = true
		sess.refs = 0
	}
	sr.mu.Unlock()
	if stop {
		close(sess.closed)
		sess.cancel()
	}
}

// Wait blocks until all recording goroutines have finished and finalized their segments.
func (sr *SegmentRecorder) Wait() {
	sr.wg.Wait()
}

// DiskAvailable returns the bytes available on the recording filesystem.
func (sr *SegmentRecorder) DiskAvailable() uint64 {
	if sr.disk == nil {
		return 0
	}
	return sr.disk.Available()
}

// Disk exposes the underlying DiskSpace monitor for callers that need to
// read the dynamic MinRequired threshold (e.g. retention emergency path).
func (sr *SegmentRecorder) Disk() *media.DiskSpace {
	return sr.disk
}

// AnyPaused returns true if any recording consumer is paused due to low disk space.
func (sr *SegmentRecorder) AnyPaused() bool {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	for _, c := range sr.consumers {
		if c.Paused() {
			return true
		}
	}
	return false
}

// CurrentSegment names an actively-recording segment file.
type CurrentSegment struct {
	Camera string
	Path   string
}

// CurrentSegmentPaths returns one CurrentSegment per consumer that
// currently has an open writer. Consumers with no open segment are
// omitted. Order is the consumer-registration order.
func (sr *SegmentRecorder) CurrentSegmentPaths() []CurrentSegment {
	sr.mu.Lock()
	consumers := make([]*media.RecordingConsumer, len(sr.consumers))
	copy(consumers, sr.consumers)
	sr.mu.Unlock()

	out := make([]CurrentSegment, 0, len(consumers))
	for _, c := range consumers {
		path := c.CurrentSegmentPath()
		if path == "" {
			continue
		}
		out = append(out, CurrentSegment{Camera: c.Camera(), Path: path})
	}
	return out
}

// ensureSegmentDir creates segDir, retrying on failure until it succeeds or
// ctx is cancelled. Returns false only if cancelled before success.
func (sr *SegmentRecorder) ensureSegmentDir(ctx context.Context, cameraName, segDir string) bool {
	const retryInterval = 5 * time.Second
	loggedFailure := false
	for {
		if err := os.MkdirAll(segDir, 0o755); err == nil {
			if loggedFailure {
				slog.Info("segment directory created, recording resuming", "camera", cameraName)
			}
			return true
		} else if !loggedFailure {
			slog.Error("failed to create segment directory, will retry",
				"camera", cameraName, "error", err)
			loggedFailure = true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(retryInterval):
		}
	}
}

func (sr *SegmentRecorder) recordLoop(ctx context.Context, cameraName, rtspURL, segDir string) {
	// Creating the segment directory can fail transiently when the recording
	// volume is briefly unavailable (just after a reboot or a USB stall).
	// Retry until it succeeds rather than abandoning the camera permanently -
	// the same self-healing philosophy as RTSP reconnect and the consumer's
	// disk-full pause/resume.
	if !sr.ensureSegmentDir(ctx, cameraName, segDir) {
		return // context cancelled before the volume came back
	}

	source := sr.hub.GetOrCreate(rtspURL)

	// Wait for connection and track info
	var videoTrack *rtsp.TrackInfo
	for {
		videoTrack = source.VideoTrack()
		if videoTrack != nil {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
	audioTrack := source.AudioTrack()

	slog.Info("starting native recording",
		"camera", cameraName,
		"video", videoTrack.Codec,
		"audio_available", audioTrack != nil,
	)

	segmentLen := sr.config.SegmentLength
	if segmentLen == 0 {
		segmentLen = 10 * time.Minute
	}

	db := sr.db
	consumer := media.NewRecordingConsumer(segDir, cameraName, segmentLen, videoTrack, audioTrack, sr.disk, func(info media.SegmentInfo) {
		rec := storage.SegmentRecord{
			Camera:    info.Camera,
			Path:      info.Path,
			StartTime: info.StartTime,
			EndTime:   info.EndTime,
			SizeBytes: info.SizeBytes,
		}
		if err := db.SaveSegment(rec); err != nil {
			slog.Error("failed to save segment to database", "path", info.Path, "error", err)
		}
	})
	// The row above is written as soon as the file is created, so the consumer
	// has to be able to retract it when it deletes that file again.
	consumer.SetSegmentRemovedHook(func(path string) {
		if err := db.DeleteSegment(path); err != nil {
			slog.Error("failed to delete segment from database", "path", path, "error", err)
		}
	})

	sr.mu.Lock()
	sr.consumers = append(sr.consumers, consumer)
	sr.mu.Unlock()

	source.AddConsumer(consumer)
	// Deferred calls run in reverse order, so this pair detaches the consumer
	// from the fan-out first and only then closes it. Closing while still
	// registered leaves the source delivering packets into a consumer that is
	// tearing down.
	defer consumer.Close()
	defer source.RemoveConsumer(consumer)
	defer func() {
		sr.mu.Lock()
		for i, c := range sr.consumers {
			if c == consumer {
				sr.consumers = append(sr.consumers[:i], sr.consumers[i+1:]...)
				break
			}
		}
		sr.mu.Unlock()
	}()

	// Block until context is cancelled — the Hub handles reconnection
	<-ctx.Done()
}

// FindSegments returns segments for a camera that overlap the given time range.
func (sr *SegmentRecorder) FindSegments(cameraName string, from, to time.Time) []Segment {
	records, err := sr.db.QuerySegments(cameraName, from, to)
	if err != nil {
		slog.Error("failed to query segments from database", "camera", cameraName, "error", err)
		return nil
	}

	segments := make([]Segment, len(records))
	for i, r := range records {
		segments[i] = Segment{
			Path:      r.Path,
			Camera:    r.Camera,
			StartTime: r.StartTime,
			EndTime:   r.EndTime,
		}
	}
	return segments
}

// RemoveSegment deletes a segment file and removes it from the database.
func (sr *SegmentRecorder) RemoveSegment(cameraName string, segPath string) error {
	if err := os.Remove(segPath); err != nil && !os.IsNotExist(err) {
		return err
	}

	if err := sr.db.DeleteSegment(segPath); err != nil {
		return fmt.Errorf("delete segment from database: %w", err)
	}
	return nil
}

// AllSegments returns all segments for a given camera.
func (sr *SegmentRecorder) AllSegments(cameraName string) []Segment {
	records, err := sr.db.GetAllSegments(cameraName)
	if err != nil {
		slog.Error("failed to get all segments from database", "camera", cameraName, "error", err)
		return nil
	}

	segments := make([]Segment, len(records))
	for i, r := range records {
		segments[i] = Segment{
			Path:      r.Path,
			Camera:    r.Camera,
			StartTime: r.StartTime,
			EndTime:   r.EndTime,
		}
	}
	return segments
}

// ScanExistingSegments reconciles the filesystem with the database for a camera.
func (sr *SegmentRecorder) ScanExistingSegments(cameraName, segDir string) {
	slog.Info("scanning existing segments", "camera", cameraName, "dir", segDir)

	// Clean up stale temp files
	tempEntries, _ := os.ReadDir(segDir)
	for _, entry := range tempEntries {
		name := entry.Name()
		if strings.HasSuffix(name, ".remux.mp4") || strings.HasSuffix(name, ".mp4.tmp") {
			tmpPath := filepath.Join(segDir, name)
			slog.Warn("removing stale temp file", "path", tmpPath)
			_ = os.Remove(tmpPath)
		}
	}

	// Build set of files on disk
	diskFiles := make(map[string]os.FileInfo)
	entries, err := os.ReadDir(segDir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Error("failed to read segment directory", "dir", segDir, "error", err)
		}
		return
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".mp4") {
			continue
		}
		fullPath := filepath.Join(segDir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		diskFiles[fullPath] = info
	}

	// Check DB records against disk: remove orphans
	dbRecords, err := sr.db.GetAllSegments(cameraName)
	if err != nil {
		slog.Error("failed to query segments from database", "camera", cameraName, "error", err)
		return
	}

	dbPaths := make(map[string]bool, len(dbRecords))
	for _, rec := range dbRecords {
		dbPaths[rec.Path] = true
		if _, exists := diskFiles[rec.Path]; !exists {
			slog.Warn("removing orphaned segment record", "camera", cameraName, "path", rec.Path)
			if err := sr.db.DeleteSegment(rec.Path); err != nil {
				slog.Error("failed to delete orphaned segment", "path", rec.Path, "error", err)
			}
		}
	}

	// Check disk files against DB: insert missing
	for path, info := range diskFiles {
		if dbPaths[path] {
			continue
		}

		duration, err := probeDuration(path)
		if err != nil {
			// A duration that cannot be read is not a duration of zero.
			// Importing it as zero writes StartTime == EndTime, which is
			// indistinguishable from a genuine zero-length segment: the row
			// then overlaps no query range, so the footage is silently
			// unreachable while still being counted. Leave the file alone and
			// skip the row.
			slog.Warn("skipping segment with unreadable duration",
				"camera", cameraName, "path", path, "error", err)
			continue
		}
		if duration <= 0 {
			// A readable file that holds no samples (a bare init segment left
			// by a create that never got a keyframe). A row for it would
			// describe an empty instant.
			slog.Warn("skipping segment with no media",
				"camera", cameraName, "path", path)
			continue
		}
		startTime := info.ModTime().Add(-duration)

		rec := storage.SegmentRecord{
			Camera:    cameraName,
			Path:      path,
			StartTime: startTime,
			EndTime:   info.ModTime(),
			SizeBytes: info.Size(),
		}

		slog.Info("importing segment from disk", "camera", cameraName, "path", path)
		if err := sr.db.SaveSegment(rec); err != nil {
			slog.Error("failed to import segment", "path", path, "error", err)
		}
	}
}

// probeDuration uses pure Go MP4 parsing to determine the duration of a video
// file. The error is returned rather than folded into a zero duration: the
// caller has to be able to tell "this file is unreadable" from "this file holds
// no video".
func probeDuration(path string) (time.Duration, error) {
	return media.ProbeDuration(path)
}
