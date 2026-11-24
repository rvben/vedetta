package recording

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/storage"
)

// ErrStorageBusy is returned when segmentOpMu cannot be acquired
// via TryLock. The HTTP layer maps this to 409 + Retry-After: 5.
var ErrStorageBusy = errors.New("storage operation in progress")

// ErrOpenSegmentProtected is returned when a delete request explicitly
// targets a currently-open segment. The HTTP layer maps this to 422
// with the protected paths in the body.
type ErrOpenSegmentProtected struct {
	Paths []string
}

func (e *ErrOpenSegmentProtected) Error() string {
	return "request targets currently-open segment(s)"
}

// FilesystemStats describes one storage root.
type FilesystemStats struct {
	UsedBytes      int64  `json:"used_bytes"`
	SegmentBytes   int64  `json:"segment_bytes,omitempty"`
	ClipBytes      int64  `json:"clip_bytes,omitempty"`
	DiskAvailable  int64  `json:"disk_available"`
	Root           string `json:"root"`
	SameFilesystem bool   `json:"same_filesystem_as_recording,omitempty"`
}

// CameraBreakdown is one row of the per-camera table.
type CameraBreakdown struct {
	Name                string     `json:"name"`
	SegmentBytes        int64      `json:"segment_bytes"`
	ClipBytes           int64      `json:"clip_bytes"`
	OldestSegment       *time.Time `json:"oldest_segment,omitempty"`
	Last7dBytes         int64      `json:"last_7d_bytes"`
	PerDay              []DayBytes `json:"per_day"`
	EffectiveRetainDays int        `json:"effective_retain_days"`
}

// DayBytes is one bar of the 30-day chart.
type DayBytes struct {
	Date  string `json:"date"` // YYYY-MM-DD, UTC
	Bytes int64  `json:"bytes"`
}

// StorageBreakdown is the response shape of GET /api/storage.
type StorageBreakdown struct {
	Recording       FilesystemStats    `json:"recording"`
	Snapshots       FilesystemStats    `json:"snapshots"`
	RecordingPaused bool               `json:"recording_paused"`
	Recompression   RecompressionStats `json:"recompression"`
	Projection      any                `json:"projection"`
	Cameras         []CameraBreakdown  `json:"cameras"`
}

// DeleteTarget enumerates the four valid delete shapes.
type DeleteTarget string

const (
	DeleteSegments  DeleteTarget = "segments"
	DeleteClips     DeleteTarget = "clips"
	DeleteAll       DeleteTarget = "all"
	DeleteFreeBytes DeleteTarget = "free_bytes"
)

// DeleteRequest is the input to Recorder.DeleteStorage.
type DeleteRequest struct {
	Target          DeleteTarget `json:"target"`
	Camera          string       `json:"camera,omitempty"`
	OlderThanDays   int          `json:"older_than_days,omitempty"`
	From            string       `json:"from,omitempty"` // YYYY-MM-DD
	To              string       `json:"to,omitempty"`   // YYYY-MM-DD
	FreeBytesTarget int64        `json:"free_bytes_target,omitempty"`
	DryRun          bool         `json:"-"` // populated from query string
}

// DeleteResult is the response shape of POST /api/storage/delete.
type DeleteResult struct {
	Segments  int      `json:"segments"`
	Clips     int      `json:"clips"`
	Snapshots int      `json:"snapshots"`
	Bytes     int64    `json:"bytes"`
	Cameras   []string `json:"cameras"`
}

// MarshalScope serializes a DeleteRequest into the JSON shape stored
// in storage_audit.scope_json.
func (req DeleteRequest) MarshalScope() string {
	b, _ := json.Marshal(req)
	return string(b)
}

// breakdownCache is the in-process 30s cache of StorageBreakdown.
type breakdownCache struct {
	mu      sync.RWMutex
	value   *StorageBreakdown
	storeAt time.Time
}

const breakdownTTL = 30 * time.Second

// StorageBreakdown returns the cached per-camera storage figures used
// by GET /api/storage. The cache TTL is 30s.
func (r *Recorder) StorageBreakdown() (*StorageBreakdown, error) {
	if v := r.cachedBreakdown(); v != nil {
		// Recompression state is cheap to read and changes independently of
		// the 30s breakdown cache. Refresh it on every read (over a shallow
		// copy, leaving the cached value untouched) so the storage page
		// reflects an in-flight pass immediately after triggering one.
		fresh := *v
		fresh.Recompression = r.recompressionStats()
		return &fresh, nil
	}

	openByCamera := make(map[string]string)
	if r.segments != nil {
		for _, s := range r.segments.CurrentSegmentPaths() {
			openByCamera[s.Camera] = s.Path
		}
	}

	out := &StorageBreakdown{
		Recording: FilesystemStats{Root: r.config.Path},
		Snapshots: FilesystemStats{Root: r.snapshotPath},
	}
	out.Recording.DiskAvailable = statfsAvailable(r.config.Path)
	out.Snapshots.DiskAvailable = statfsAvailable(r.snapshotPath)
	out.Snapshots.SameFilesystem = sameFilesystem(r.config.Path, r.snapshotPath)

	for _, cam := range r.cameraNames() {
		cb := r.cameraBreakdown(cam, openByCamera[cam])
		out.Cameras = append(out.Cameras, cb)
		out.Recording.SegmentBytes += cb.SegmentBytes
		out.Recording.ClipBytes += cb.ClipBytes
	}
	out.Recording.UsedBytes = out.Recording.SegmentBytes + out.Recording.ClipBytes
	out.Snapshots.UsedBytes = r.totalSnapshotBytes()
	out.RecordingPaused = r.recordingPaused()
	out.Recompression = r.recompressionStats()
	out.Projection = r.projection()

	r.storeBreakdown(out)
	return out, nil
}

func (r *Recorder) cachedBreakdown() *StorageBreakdown {
	r.breakdownCache.mu.RLock()
	defer r.breakdownCache.mu.RUnlock()
	if r.breakdownCache.value != nil && time.Since(r.breakdownCache.storeAt) < breakdownTTL {
		return r.breakdownCache.value
	}
	return nil
}

func (r *Recorder) storeBreakdown(v *StorageBreakdown) {
	r.breakdownCache.mu.Lock()
	defer r.breakdownCache.mu.Unlock()
	r.breakdownCache.value = v
	r.breakdownCache.storeAt = time.Now()
}

func (r *Recorder) cameraNames() []string {
	names := make([]string, 0, len(r.cameraURLs))
	for name := range r.cameraURLs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *Recorder) effectiveRetainDays(cam string) int {
	if v, ok := r.cameraRetention[cam]; ok {
		return v
	}
	return r.config.RetainDays
}

func (r *Recorder) recordingPaused() bool {
	if r.segments == nil {
		return false
	}
	return r.segments.AnyPaused()
}

func (r *Recorder) projection() any {
	if r.segments == nil {
		return nil
	}
	stats := r.StorageStats()
	return r.computeProjection(&stats)
}

// cameraBreakdown computes one row, including a stat of the currently
// open segment (if any) so the live segment isn't reported as 0 bytes.
func (r *Recorder) cameraBreakdown(camera, openPath string) CameraBreakdown {
	cb := CameraBreakdown{
		Name:                camera,
		EffectiveRetainDays: r.effectiveRetainDays(camera),
	}
	if r.db != nil {
		farPast := time.Unix(0, 0)
		farFuture := time.Now().AddDate(100, 0, 0)
		if segs, err := r.db.SegmentsByCameraInRange(camera, farPast, farFuture); err == nil {
			for _, s := range segs {
				cb.SegmentBytes += s.SizeBytes
				if cb.OldestSegment == nil || s.StartTime.Before(*cb.OldestSegment) {
					ts := s.StartTime
					cb.OldestSegment = &ts
				}
			}
		}
	}
	if openPath != "" {
		if fi, err := os.Stat(openPath); err == nil {
			cb.SegmentBytes += fi.Size()
		}
	}
	cb.ClipBytes = r.clipBytesByCamera()[camera]
	cb.PerDay = r.perDayBytes(camera, 30)
	cutoff := len(cb.PerDay) - 7
	if cutoff < 0 {
		cutoff = 0
	}
	for _, d := range cb.PerDay[cutoff:] {
		cb.Last7dBytes += d.Bytes
	}
	return cb
}

// clipBytesCache holds the per-camera clip sizes. Sizing clips means walking
// every clip file, so the walk is shared by the 10s stats refresh and the
// storage breakdown instead of running once for each.
type clipBytesCache struct {
	mu      sync.Mutex
	value   map[string]int64
	storeAt time.Time
}

const clipBytesTTL = time.Minute

// clipBytesByCamera returns the bytes of event clips on disk per registered
// camera. Callers must not modify the returned map.
func (r *Recorder) clipBytesByCamera() map[string]int64 {
	c := &r.clipBytesCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.value != nil && time.Since(c.storeAt) < clipBytesTTL {
		return c.value
	}
	out := make(map[string]int64)
	for _, cam := range r.cameraNames() {
		var sum int64
		_ = filepath.WalkDir(filepath.Join(r.config.Path, cam, "clips"), func(_ string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if fi, err := d.Info(); err == nil {
				sum += fi.Size()
			}
			return nil
		})
		out[cam] = sum
	}
	c.value = out
	c.storeAt = time.Now()
	return out
}

func (r *Recorder) perDayBytes(camera string, days int) []DayBytes {
	if r.db == nil {
		return nil
	}
	rows, err := r.db.PerDayCameraSegmentBytes(camera, days)
	if err != nil {
		return nil
	}
	out := make([]DayBytes, 0, len(rows))
	for _, row := range rows {
		out = append(out, DayBytes(row))
	}
	return out
}

func (r *Recorder) totalSnapshotBytes() int64 {
	if r.db == nil {
		return 0
	}
	paths, err := r.db.AllSnapshotPaths()
	if err != nil {
		return 0
	}
	var sum int64
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil {
			sum += fi.Size()
		}
	}
	return sum
}

func statfsAvailable(root string) int64 {
	var s syscall.Statfs_t
	if err := syscall.Statfs(root, &s); err != nil {
		return 0
	}
	return int64(s.Bavail) * int64(s.Bsize)
}

func sameFilesystem(a, b string) bool {
	var sa, sb syscall.Statfs_t
	if err := syscall.Statfs(a, &sa); err != nil {
		return false
	}
	if err := syscall.Statfs(b, &sb); err != nil {
		return false
	}
	return sa.Fsid == sb.Fsid
}

// DeleteStorage implements POST /api/storage/delete. It validates the
// request, acquires segmentOpMu via TryLock (returns ErrStorageBusy on
// failure), excludes any currently-open segment by path equality
// (returns ErrOpenSegmentProtected if the request explicitly targets
// one), and removes files + DB rows in the same order as RemoveSegment
// (FS first, then DB).
func (r *Recorder) DeleteStorage(req DeleteRequest) (*DeleteResult, error) {
	if err := validateDeleteRequest(req); err != nil {
		return nil, err
	}
	if !req.DryRun {
		if !r.segmentOpMu.TryLock() {
			return nil, ErrStorageBusy
		}
		defer r.segmentOpMu.Unlock()
	}

	var openPaths map[string]struct{}
	if r.segments != nil {
		openPaths = pathSet(r.segments.CurrentSegmentPaths())
	} else {
		openPaths = map[string]struct{}{}
	}

	switch req.Target {
	case DeleteSegments:
		return r.deleteSegmentsScoped(req, openPaths)
	case DeleteClips:
		return r.deleteClipsScoped(req, openPaths)
	case DeleteAll:
		return r.deleteAllScoped(req, openPaths)
	case DeleteFreeBytes:
		return r.deleteFreeBytes(req, openPaths)
	}
	return nil, fmt.Errorf("recording: unreachable target %q", req.Target)
}

func validateDeleteRequest(req DeleteRequest) error {
	switch req.Target {
	case DeleteSegments, DeleteAll:
		if req.Camera == "" {
			return fmt.Errorf("camera required for target=%q", req.Target)
		}
		if !hasWindow(req) {
			return fmt.Errorf("window required for target=%q", req.Target)
		}
	case DeleteClips:
		if req.Camera == "" {
			return fmt.Errorf("camera required for target=clips")
		}
	case DeleteFreeBytes:
		if req.FreeBytesTarget <= 0 {
			return fmt.Errorf("free_bytes_target must be > 0")
		}
		if req.Camera != "" || hasWindow(req) {
			return fmt.Errorf("free_bytes target accepts no camera or window")
		}
	default:
		return fmt.Errorf("invalid target %q", req.Target)
	}
	hasOTD := req.OlderThanDays > 0
	hasRange := req.From != "" && req.To != ""
	if hasOTD && hasRange {
		return fmt.Errorf("set either older_than_days or from/to, not both")
	}
	if (req.From != "") != (req.To != "") {
		return fmt.Errorf("set both from and to or neither")
	}
	return nil
}

func hasWindow(req DeleteRequest) bool {
	return req.OlderThanDays > 0 || (req.From != "" && req.To != "")
}

func pathSet(segs []CurrentSegment) map[string]struct{} {
	m := make(map[string]struct{}, len(segs))
	for _, s := range segs {
		m[s.Path] = struct{}{}
	}
	return m
}

func (r *Recorder) deleteSegmentsScoped(req DeleteRequest, openPaths map[string]struct{}) (*DeleteResult, error) {
	from, to, err := resolveWindow(req)
	if err != nil {
		return nil, err
	}
	segs, err := r.db.SegmentsByCameraInRange(req.Camera, from, to)
	if err != nil {
		return nil, err
	}
	res := &DeleteResult{Cameras: []string{req.Camera}}
	var protected []string
	for _, s := range segs {
		if _, isOpen := openPaths[s.Path]; isOpen {
			protected = append(protected, s.Path)
			continue
		}
		if req.DryRun {
			res.Segments++
			res.Bytes += s.SizeBytes
			continue
		}
		if err := r.removeSegmentFile(s.Path); err == nil {
			if err := r.db.DeleteSegmentByID(s.ID); err == nil {
				res.Segments++
				res.Bytes += s.SizeBytes
			}
		}
	}
	if len(protected) > 0 && !req.DryRun {
		return nil, &ErrOpenSegmentProtected{Paths: protected}
	}
	return res, nil
}

func resolveWindow(req DeleteRequest) (from, to time.Time, err error) {
	if req.OlderThanDays > 0 {
		to = time.Now().UTC().AddDate(0, 0, -req.OlderThanDays)
		from = time.Unix(0, 0)
		return from, to, nil
	}
	fromDay, err := time.Parse("2006-01-02", req.From)
	if err != nil {
		return from, to, fmt.Errorf("invalid from: %w", err)
	}
	toDay, err := time.Parse("2006-01-02", req.To)
	if err != nil {
		return from, to, fmt.Errorf("invalid to: %w", err)
	}
	if toDay.Before(fromDay) {
		return from, to, fmt.Errorf("to must be >= from")
	}
	return fromDay.UTC(), toDay.AddDate(0, 0, 1).UTC(), nil
}

// clipRefClearer is the subset of *storage.DB used to drop database references
// to clip and snapshot files once they are deleted from disk. *storage.DB
// satisfies it.
type clipRefClearer interface {
	ClearEventClip(eventID string) error
	UpdateEventSnapshotPath(eventID, snapshotPath string) error
	UpdateEventSnapshotAvailability(eventID string, available bool) error
}

// clearClipRef removes the DB reference to a deleted clip file, zeroing the
// clip path, availability, size, and recompression state in one write.
func clearClipRef(db clipRefClearer, eventID string) error {
	return db.ClearEventClip(eventID)
}

// clearSnapshotRef removes the DB reference to a deleted snapshot file.
func clearSnapshotRef(db clipRefClearer, eventID string) error {
	return errors.Join(
		db.UpdateEventSnapshotPath(eventID, ""),
		db.UpdateEventSnapshotAvailability(eventID, false),
	)
}

func (r *Recorder) removeSegmentFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (r *Recorder) deleteClipsScoped(req DeleteRequest, _ map[string]struct{}) (*DeleteResult, error) {
	events, err := r.fetchEventsForDelete(req)
	if err != nil {
		return nil, err
	}
	res := &DeleteResult{Cameras: []string{req.Camera}}
	var refErrs []error
	for _, ev := range events {
		clipBytes := statSize(ev.ClipPath)
		snapBytes := statSize(ev.SnapshotPath)
		if req.DryRun {
			if ev.ClipPath != "" {
				res.Clips++
				res.Bytes += clipBytes
			}
			if ev.SnapshotPath != "" {
				res.Snapshots++
				res.Bytes += snapBytes
			}
			continue
		}
		if ev.ClipPath != "" {
			if err := os.Remove(ev.ClipPath); err == nil || errors.Is(err, os.ErrNotExist) {
				if err := clearClipRef(r.db, ev.ID); err != nil {
					refErrs = append(refErrs, fmt.Errorf("event %s clip ref: %w", ev.ID, err))
				}
				res.Clips++
				res.Bytes += clipBytes
			}
		}
		if ev.SnapshotPath != "" {
			if err := os.Remove(ev.SnapshotPath); err == nil || errors.Is(err, os.ErrNotExist) {
				if err := clearSnapshotRef(r.db, ev.ID); err != nil {
					refErrs = append(refErrs, fmt.Errorf("event %s snapshot ref: %w", ev.ID, err))
				}
				res.Snapshots++
				res.Bytes += snapBytes
			}
		}
	}
	if len(refErrs) > 0 {
		// The files are gone but their DB rows still reference them: surface
		// the inconsistency rather than reporting a clean success.
		joined := errors.Join(refErrs...)
		slog.Error("deleted media files but failed to clear DB references",
			"camera", req.Camera, "error", joined)
		return res, fmt.Errorf("clearing DB references for deleted media: %w", joined)
	}
	return res, nil
}

// fetchEventsForDelete returns the events matching the delete request's
// camera and optional time window. When no window is specified, all events
// for the camera are returned.
func (r *Recorder) fetchEventsForDelete(req DeleteRequest) ([]camera.Event, error) {
	if req.OlderThanDays == 0 && req.From == "" && req.To == "" {
		return r.db.ClipsByCamera(req.Camera)
	}
	from, to, err := resolveWindow(req)
	if err != nil {
		return nil, err
	}
	if req.OlderThanDays > 0 {
		return r.db.ClipsByCameraOlderThan(req.Camera, to)
	}
	return r.db.ClipsByCameraInRange(req.Camera, from, to)
}

// statSize returns the size of the file at path, or 0 if the file does not
// exist or cannot be stat'd.
func statSize(path string) int64 {
	if path == "" {
		return 0
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func (r *Recorder) deleteAllScoped(req DeleteRequest, openPaths map[string]struct{}) (*DeleteResult, error) {
	segReq := req
	segReq.Target = DeleteSegments
	segRes, err := r.deleteSegmentsScoped(segReq, openPaths)
	if err != nil {
		return nil, err
	}

	clipReq := req
	clipReq.Target = DeleteClips
	clipRes, err := r.deleteClipsScoped(clipReq, openPaths)
	if err != nil {
		return nil, err
	}

	return &DeleteResult{
		Segments:  segRes.Segments,
		Clips:     clipRes.Clips,
		Snapshots: clipRes.Snapshots,
		Bytes:     segRes.Bytes + clipRes.Bytes,
		Cameras:   []string{req.Camera},
	}, nil
}

func (r *Recorder) deleteFreeBytes(req DeleteRequest, openPaths map[string]struct{}) (*DeleteResult, error) {
	segs, err := r.db.OldestSegmentsUntilBytes(req.FreeBytesTarget)
	if err != nil {
		return nil, err
	}
	res := &DeleteResult{}
	cameras := make(map[string]struct{})
	for _, s := range segs {
		if _, isOpen := openPaths[s.Path]; isOpen {
			continue
		}
		if req.DryRun {
			res.Segments++
			res.Bytes += s.SizeBytes
			cameras[s.Camera] = struct{}{}
			continue
		}
		if err := r.removeSegmentFile(s.Path); err == nil {
			if err := r.db.DeleteSegmentByID(s.ID); err == nil {
				res.Segments++
				res.Bytes += s.SizeBytes
				cameras[s.Camera] = struct{}{}
			}
		}
	}
	for c := range cameras {
		res.Cameras = append(res.Cameras, c)
	}
	sort.Strings(res.Cameras)
	return res, nil
}

// TryRunCleanupAsync starts a background retention pass. Returns
// ErrStorageBusy if segmentOpMu cannot be acquired. The spawned
// goroutine owns the lock for the duration of the run.
func (r *Recorder) TryRunCleanupAsync() error {
	if !r.segmentOpMu.TryLock() {
		return ErrStorageBusy
	}
	go func() {
		defer r.segmentOpMu.Unlock()
		r.runCleanupLocked()
	}()
	return nil
}

// RecordStorageAudit writes one audit row after a successful manual
// delete. Errors are returned but the caller usually only logs them.
func (r *Recorder) RecordStorageAudit(actor, scopeJSON string, bytes int64, files int) error {
	return r.db.InsertStorageAudit(storage.StorageAuditEntry{
		Timestamp: time.Now().UTC(),
		Actor:     actor,
		ScopeJSON: scopeJSON,
		Bytes:     bytes,
		Files:     files,
	})
}

// StorageAudit returns the most recent N audit rows for GET /api/storage/audit.
func (r *Recorder) StorageAudit(limit int) ([]storage.StorageAuditEntry, error) {
	return r.db.StorageAudit(limit)
}
