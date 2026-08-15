package recording

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/media"
	"github.com/rvben/vedetta/internal/rtsp"
	"github.com/rvben/vedetta/internal/safepath"
	"github.com/rvben/vedetta/internal/snapshot"
	"github.com/rvben/vedetta/internal/storage"
)

// StorageStats contains aggregate storage information.
type StorageStats struct {
	TotalBytes      int64              `json:"total_bytes"`
	SegmentCount    int                `json:"segment_count"`
	CameraStats     map[string]int64   `json:"camera_stats"`
	DiskAvailable   uint64             `json:"disk_available_bytes"`
	DiskLow         bool               `json:"disk_low"`
	RecordingPaused bool               `json:"recording_paused"`
	Recompression   RecompressionStats `json:"recompression"`
	Projection      StorageProjection  `json:"projection"`
}

// RecompressionStats summarises the tiered storage recompression feature.
type RecompressionStats struct {
	Enabled              bool      `json:"enabled"`
	IsRunning            bool      `json:"is_running"`
	LastRun              time.Time `json:"last_run,omitempty"`
	SegmentsRecompressed int64     `json:"segments_recompressed"`
	ClipsRecompressed    int64     `json:"clips_recompressed"`
	BytesReclaimed       int64     `json:"bytes_reclaimed"`
	TranscodeFailures    int64     `json:"transcode_failures"`
	WorkerCrashes        int64     `json:"worker_crashes"`
	WorkerTimeouts       int64     `json:"worker_timeouts"`
}

// StorageProjection projects future storage usage based on current ingest
// rate, retention config, and observed segment history. Helps answer
// "will my config fit?" and "how long until disk is full?" before limits
// are actually hit.
type StorageProjection struct {
	// DailyIngestBytes is the observed ingest rate computed from segments
	// written in the last 24 hours.
	DailyIngestBytes int64 `json:"daily_ingest_bytes"`

	// SteadyStateBytes is the predicted total storage usage once retention
	// is fully cycling (daily ingest × retain days, plus a small buffer).
	SteadyStateBytes int64 `json:"steady_state_bytes"`

	// SteadyStateFits is true when the projected steady state will fit on
	// the disk with a 5% safety margin.
	SteadyStateFits bool `json:"steady_state_fits"`

	// HeadroomBytes is the disk space remaining after steady state. Negative
	// values mean the config will not fit.
	HeadroomBytes int64 `json:"headroom_bytes"`

	// DaysUntilFull is a linear projection of days until the disk fills at
	// the current ingest rate, assuming no retention cleanup kicks in.
	// Only set while the system is still filling (oldest segment younger
	// than retain_days). Negative means already full.
	DaysUntilFull *float64 `json:"days_until_full,omitempty"`

	// OldestSegmentDays is the age in days of the oldest segment in the
	// database. When this exceeds RetainDays, the system is at steady state.
	OldestSegmentDays float64 `json:"oldest_segment_days"`

	// RetainDays is the configured continuous recording retention.
	RetainDays int `json:"retain_days"`

	// Status is a human-readable summary: ok, warning, insufficient, critical.
	// - ok: steady state fits comfortably (<85% of disk)
	// - warning: steady state uses 85-95% of disk OR <14 days until full
	// - insufficient: steady state will not fit — config is broken
	// - critical: disk is already full (disk_low flag tripped)
	Status string `json:"status"`
}

// Recorder manages saving video clips for detected events.
type Recorder struct {
	config               config.RecordingConfig
	eventConfig          config.EventConfig
	db                   *storage.DB
	hub                  *rtsp.Hub
	segments             *SegmentRecorder
	recompressor         *Recompressor
	cameraURLs           map[string]string // camera name → record RTSP URL
	cameraRetention      map[string]int    // camera name → retain_days override (only cameras with explicit overrides)
	startTime            time.Time
	snapshotPath         string
	snapshotFallbackPath string
	snapshotSaver        *snapshot.Saver
	exportProcess        func(inputs []string, outputPath string, start, duration time.Duration) error

	// segmentOpMu serializes every operation that creates, renames, or
	// deletes a segment, clip, or snapshot file. Callers that perform
	// any of those file operations must hold this mutex.
	segmentOpMu sync.Mutex

	// Cached storage stats refreshed in background
	statsMu     sync.RWMutex
	cachedStats StorageStats

	breakdownCache breakdownCache
}

func New(cfg config.RecordingConfig, eventCfg config.EventConfig, cameras []config.CameraConfig, db *storage.DB, hub *rtsp.Hub, snapshotPath, snapshotFallbackPath string, snapshotSaver *snapshot.Saver) *Recorder {
	if err := os.MkdirAll(cfg.Path, 0o755); err != nil {
		slog.Error("failed to create recording directory", "path", cfg.Path, "error", err)
	}

	// Clean up stale export temp files from previous runs
	exportDir := filepath.Join(cfg.Path, ".exports")
	os.RemoveAll(exportDir)

	r := &Recorder{
		config:               cfg,
		eventConfig:          eventCfg,
		db:                   db,
		hub:                  hub,
		segments:             NewSegmentRecorder(cfg, db, hub),
		cameraURLs:           make(map[string]string),
		cameraRetention:      buildCameraRetention(cameras),
		startTime:            time.Now(),
		snapshotPath:         snapshotPath,
		snapshotFallbackPath: snapshotFallbackPath,
		snapshotSaver:        snapshotSaver,
		exportProcess: func(inputs []string, outputPath string, start, duration time.Duration) error {
			if len(inputs) == 1 {
				return media.TrimMP4(inputs[0], outputPath, start, duration)
			}
			return media.ConcatMP4(inputs, outputPath, start, duration)
		},
	}
	r.recompressor = NewRecompressor(cfg.TieredStorage, cameras, db, &r.segmentOpMu)
	return r
}

// RegisterCamera registers a camera's recording URL for direct-from-stream recording.
func (r *Recorder) RegisterCamera(name, rtspURL string) {
	r.cameraURLs[name] = rtspURL
}

// CameraURL returns the recording URL for a camera, or empty string if not registered.
func (r *Recorder) CameraURL(name string) string {
	return r.cameraURLs[name]
}

// StartTemporaryRecording begins segment recording for a single camera.
// Used when continuous recording is off but an event requires video.
// Cancel the context to stop recording.
func (r *Recorder) StartTemporaryRecording(ctx context.Context, cameraName, rtspURL string) {
	r.segments.StartRecording(ctx, cameraName, rtspURL)
}

// StopCameraRecording stops recording for a single camera.
func (r *Recorder) StopCameraRecording(name string) {
	r.segments.StopRecording(name)
}

// StartCameraRecording starts recording for a single camera.
func (r *Recorder) StartCameraRecording(ctx context.Context, name string) {
	url, ok := r.cameraURLs[name]
	if !ok {
		slog.Warn("no recording URL registered for camera", "camera", name)
		return
	}
	r.segments.StartRecording(ctx, name, url)
}

// StartContinuousRecording begins segment recording for all registered cameras
// that are not in the stoppedCameras set.
func (r *Recorder) StartContinuousRecording(ctx context.Context, stoppedCameras map[string]bool) {
	if !r.config.Continuous {
		slog.Info("continuous recording disabled")
		return
	}

	first := true
	for name, url := range r.cameraURLs {
		if stoppedCameras[name] {
			continue
		}
		if !first {
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
		r.segments.StartRecording(ctx, name, url)
		first = false
	}

	// Reconcile filesystem with database in the background to avoid blocking startup.
	go func() {
		for name := range r.cameraURLs {
			segDir, err := safepath.Join(r.config.Path, name, "segments")
			if err != nil {
				slog.Error("invalid segment scan directory", "camera", name, "error", err)
				continue
			}
			r.segments.ScanExistingSegments(name, segDir)
		}
	}()

	slog.Info("continuous recording started", "cameras", len(r.cameraURLs))
}

// SaveClip extracts a clip around the event timestamp and persists its
// path on the event row. Blocks on segmentOpMu so a concurrent manual
// delete cannot run between ExtractClip and SetEventClip.
func (r *Recorder) SaveClip(ctx context.Context, event camera.Event) (ClipStats, error) {
	r.segmentOpMu.Lock()
	defer r.segmentOpMu.Unlock()
	return r.saveClipLocked(ctx, event)
}

func (r *Recorder) saveClipLocked(ctx context.Context, event camera.Event) (ClipStats, error) {
	clipPath, stats, err := r.ExtractClip(ctx, event)
	if err != nil {
		return stats, fmt.Errorf("extract clip: %w", err)
	}

	if err := r.db.SetEventClip(event.ID, clipPath, stats.OutputBytes); err != nil {
		slog.Error("failed to record event clip", "error", err)
	}

	slog.Info("clip saved",
		"camera", event.CameraName,
		"label", event.Label,
		"path", clipPath,
	)

	return stats, nil
}

// Close waits for all recording goroutines to finalize their segments,
// with a timeout to prevent hanging on shutdown.
func (r *Recorder) Close() {
	done := make(chan struct{})
	go func() {
		r.segments.Wait()
		close(done)
	}()
	select {
	case <-done:
		slog.Info("all recording segments finalized")
	case <-time.After(10 * time.Second):
		slog.Warn("timed out waiting for recording segments to finalize")
	}
}

// StartRecompressionJob begins the tiered storage recompression goroutine.
func (r *Recorder) StartRecompressionJob(ctx context.Context) {
	r.recompressor.Start(ctx)
}

// StartStatsRefresh begins a background loop that periodically refreshes
// cached storage stats. This prevents API handlers from blocking on DB queries.
func (r *Recorder) StartStatsRefresh(ctx context.Context) {
	go func() {
		// Initial refresh in the goroutine to avoid blocking startup
		// when segment scanning holds the DB connection.
		r.RefreshStats()

		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.RefreshStats()
			}
		}
	}()
}

// RefreshStats queries the database and updates the cached stats.
func (r *Recorder) RefreshStats() {
	stats := StorageStats{
		CameraStats: make(map[string]int64),
	}

	totalBytes, err := r.db.TotalSegmentBytes()
	if err != nil {
		slog.Error("failed to query total segment bytes", "error", err)
	} else {
		stats.TotalBytes = totalBytes
	}

	count, err := r.db.CountSegments()
	if err != nil {
		slog.Error("failed to query segment count", "error", err)
	} else {
		stats.SegmentCount = count
	}

	byCamera, err := r.db.SegmentBytesByCamera()
	if err != nil {
		slog.Error("failed to query segment bytes by camera", "error", err)
	} else {
		stats.CameraStats = byCamera
	}

	stats.DiskAvailable = r.segments.DiskAvailable()
	stats.DiskLow = stats.DiskAvailable < r.segments.Disk().MinRequired()
	stats.RecordingPaused = r.segments.AnyPaused()
	stats.Recompression = r.recompressionStats()

	stats.Projection = r.computeProjection(&stats)

	r.statsMu.Lock()
	r.cachedStats = stats
	r.statsMu.Unlock()
}

// computeProjection builds a StorageProjection from the current stats and
// recent segment history. Must be called with all stats fields already
// populated (TotalBytes, DiskAvailable, DiskLow).
func (r *Recorder) computeProjection(stats *StorageStats) StorageProjection {
	proj := StorageProjection{
		RetainDays: r.config.RetainDays,
		Status:     "ok",
	}

	if r.config.RetainDays <= 0 {
		// No retention configured — can't project
		return proj
	}

	// Recent ingest rate: bytes added in the last 24h
	cutoff := time.Now().Add(-24 * time.Hour)
	recentBytes, err := r.db.SegmentBytesSince(cutoff)
	if err != nil {
		slog.Debug("projection: failed to query recent bytes", "error", err)
		return proj
	}
	proj.DailyIngestBytes = recentBytes

	// Oldest segment age
	oldest, err := r.db.OldestSegmentTime()
	if err != nil || oldest.IsZero() {
		// No segments yet or error — can't project further
		return proj
	}
	proj.OldestSegmentDays = time.Since(oldest).Hours() / 24.0

	// Steady state: daily ingest × retain days
	proj.SteadyStateBytes = recentBytes * int64(r.config.RetainDays)

	totalDisk := int64(stats.DiskAvailable) + stats.TotalBytes
	if totalDisk <= 0 {
		return proj
	}

	const safetyMargin = 0.95
	safeCapacity := int64(float64(totalDisk) * safetyMargin)
	proj.HeadroomBytes = safeCapacity - proj.SteadyStateBytes
	proj.SteadyStateFits = proj.HeadroomBytes >= 0

	// Days until full (linear): only meaningful while still filling
	stillFilling := proj.OldestSegmentDays < float64(r.config.RetainDays)
	if stillFilling && recentBytes > 0 {
		remaining := safeCapacity - stats.TotalBytes
		days := float64(remaining) / float64(recentBytes)
		proj.DaysUntilFull = &days
	}

	// Status classification
	switch {
	case stats.DiskLow:
		proj.Status = "critical"
	case !proj.SteadyStateFits:
		proj.Status = "insufficient"
	case float64(proj.SteadyStateBytes) > float64(safeCapacity)*0.90:
		// Steady state > 85% of disk (since safeCapacity is already 95% of total)
		proj.Status = "warning"
	case proj.DaysUntilFull != nil && *proj.DaysUntilFull < 14:
		proj.Status = "warning"
	}

	return proj
}

// StorageStats returns cached aggregate storage information.
// Updated in background every 10s by StartStatsRefresh.
func (r *Recorder) StorageStats() StorageStats {
	r.statsMu.RLock()
	defer r.statsMu.RUnlock()
	return r.cachedStats
}

// recompressionStats snapshots the tiered storage recompression state. It is
// segment-independent, so it is safe to call from StorageBreakdown even when
// the segment store is absent.
func (r *Recorder) recompressionStats() RecompressionStats {
	if r.recompressor == nil {
		return RecompressionStats{Enabled: r.config.TieredStorage.Enabled}
	}
	rStats := r.recompressor.Stats()
	return RecompressionStats{
		Enabled:              r.config.TieredStorage.Enabled,
		IsRunning:            rStats.IsRunning,
		LastRun:              rStats.LastRun,
		SegmentsRecompressed: rStats.SegmentsRecompressed,
		ClipsRecompressed:    rStats.ClipsRecompressed,
		BytesReclaimed:       rStats.BytesReclaimed,
		TranscodeFailures:    rStats.TranscodeFailures,
		WorkerCrashes:        rStats.WorkerCrashes,
		WorkerTimeouts:       rStats.WorkerTimeouts,
	}
}

// DiskAvailable returns the bytes available on the recording filesystem.
func (r *Recorder) DiskAvailable() uint64 {
	return r.segments.DiskAvailable()
}

// AnyCameraPaused returns true if any recording consumer has paused due to disk pressure.
func (r *Recorder) AnyCameraPaused() bool {
	return r.segments.AnyPaused()
}

// DiskMonitorSampler returns the DiskSampler used by DiskMonitor. Convenience
// accessor so main.go doesn't need to reach through r.segments.Disk().
func (r *Recorder) DiskMonitorSampler() DiskSampler {
	return r.segments.Disk()
}

// HasSegments returns true if there are any segments covering the given time for a camera.
func (r *Recorder) HasSegments(cameraName string, t time.Time) bool {
	// Check a small window around the timestamp
	from := t.Add(-1 * time.Second)
	to := t.Add(1 * time.Second)
	segments := r.segments.FindSegments(cameraName, from, to)
	return len(segments) > 0
}

// ExportResult holds a prepared export ready to be served via http.ServeContent.
// The caller must call Close when done.
type ExportResult struct {
	File    *os.File
	tmpPath string
}

// Close cleans up the file handle and temporary export file.
func (er *ExportResult) Close() {
	er.File.Close()
	os.Remove(er.tmpPath)
}

// PrepareExport builds an MP4 covering [from, to) for a camera and returns
// a result that can be streamed. This separates validation/preparation
// (which can fail with a proper error) from streaming (which happens after
// HTTP headers are sent).
func (r *Recorder) PrepareExport(ctx context.Context, cameraName string, from, to time.Time) (*ExportResult, error) {
	segments := r.segments.FindSegments(cameraName, from, to)
	if len(segments) == 0 {
		return nil, fmt.Errorf("no segments found for camera %q in range %s–%s", cameraName, from.Format(time.RFC3339), to.Format(time.RFC3339))
	}

	// Filter out segments whose files no longer exist
	valid := segments[:0]
	for _, seg := range segments {
		if _, err := os.Stat(seg.Path); err == nil {
			valid = append(valid, seg)
		}
	}
	segments = valid
	if len(segments) == 0 {
		return nil, fmt.Errorf("segments deleted before export for camera %q", cameraName)
	}

	startOffset := from.Sub(segments[0].StartTime)
	if startOffset < 0 {
		startOffset = 0
	}
	duration := to.Sub(from)

	exportDir := filepath.Join(r.config.Path, ".exports")
	if err := os.MkdirAll(exportDir, 0o755); err != nil {
		return nil, fmt.Errorf("create export dir: %w", err)
	}
	tmpFile, err := os.CreateTemp(exportDir, "export-*.mp4")
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()

	inputs := make([]string, len(segments))
	for i, seg := range segments {
		inputs[i] = seg.Path
	}

	// Run trim/concat with a timeout to prevent hanging on corrupt segments.
	// Generous limit: 2 minutes per input segment.
	timeout := time.Duration(len(inputs)) * 2 * time.Minute
	type processResult struct {
		err error
	}
	resultCh := make(chan processResult, 1)
	abandoned := make(chan struct{})
	go func() {
		err := r.exportProcess(inputs, tmpPath, startOffset, duration)
		// Check abandoned first: if the caller already gave up, clean up
		// the output file rather than sending a result nobody will read.
		// A non-blocking check avoids the random select between two ready
		// channels (resultCh is buffered, so both branches would be ready).
		select {
		case <-abandoned:
			_ = os.Remove(tmpPath)
			return
		default:
		}
		select {
		case resultCh <- processResult{err: err}:
		case <-abandoned:
			_ = os.Remove(tmpPath)
		}
	}()

	var cancel <-chan struct{}
	if ctx != nil {
		cancel = ctx.Done()
	}

	select {
	case result := <-resultCh:
		if result.err != nil {
			os.Remove(tmpPath)
			return nil, fmt.Errorf("process segments: %w", result.err)
		}
	case <-time.After(timeout):
		close(abandoned)
		os.Remove(tmpPath)
		return nil, fmt.Errorf("export timed out after %s (possible corrupt segment)", timeout)
	case <-cancel:
		close(abandoned)
		os.Remove(tmpPath)
		return nil, context.Canceled
	}

	info, err := os.Stat(tmpPath)
	if err != nil {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("stat export: %w", err)
	}
	if info.Size() == 0 {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("no video data in the requested range for camera %q", cameraName)
	}

	f, err := os.Open(tmpPath)
	if err != nil {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("open export result: %w", err)
	}

	return &ExportResult{
		File:    f,
		tmpPath: tmpPath,
	}, nil
}

// TriggerRecompression starts a full recompression pass in the background.
// Returns an error if a pass is already running or tiered storage is disabled.
func (r *Recorder) TriggerRecompression(ctx context.Context) error {
	if !r.config.TieredStorage.Enabled {
		return fmt.Errorf("tiered storage is not enabled")
	}
	if r.recompressor.isRunning.Load() {
		return fmt.Errorf("recompression already running")
	}
	go r.recompressor.RunNow(ctx)
	return nil
}

// buildCameraRetention constructs a map from camera name to retain_days for
// cameras that have an explicit per-camera override set.
func buildCameraRetention(cams []config.CameraConfig) map[string]int {
	m := make(map[string]int, len(cams))
	for _, c := range cams {
		if c.RetainDays != nil && *c.RetainDays > 0 {
			m[c.Name] = *c.RetainDays
		}
	}
	return m
}

// ReextractClip removes the old clip file (if any), clears its
// availability flag, then re-extracts. The entire sequence runs under
// a single segmentOpMu acquisition so a concurrent manual delete pass
// cannot observe a half-renamed state.
func (r *Recorder) ReextractClip(ctx context.Context, event camera.Event) error {
	r.segmentOpMu.Lock()
	defer r.segmentOpMu.Unlock()

	if event.ClipPath != "" {
		if err := os.Remove(event.ClipPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("re-extract: remove old clip", "path", event.ClipPath, "error", err)
		}
		if err := r.db.UpdateEventClipAvailability(event.ID, false); err != nil {
			return fmt.Errorf("clear clip availability: %w", err)
		}
	}
	_, err := r.saveClipLocked(ctx, event)
	return err
}
