package recording

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/rvben/vedetta/internal/media"
)

// StartRetentionCleanup runs a background goroutine that periodically
// removes recordings older than the configured retention period.
// When disk space is critically low, cleanup runs every 30 seconds
// instead of every hour to recover space as quickly as possible.
func (r *Recorder) StartRetentionCleanup(ctx context.Context) {
	go func() {
		r.runCleanup()

		normalTicker := time.NewTicker(1 * time.Hour)
		urgentTicker := time.NewTicker(30 * time.Second)
		defer normalTicker.Stop()
		defer urgentTicker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-normalTicker.C:
				r.runCleanup()
			case <-urgentTicker.C:
				if r.segments.DiskAvailable() < media.MinDiskSpace {
					slog.Warn("urgent retention cleanup triggered by low disk space")
					r.runCleanup()
				}
			}
		}
	}()
}

func (r *Recorder) runCleanup() {
	slog.Debug("running retention cleanup")

	segmentCutoff := time.Now().Add(-time.Duration(r.config.RetainDays) * 24 * time.Hour)
	eventCutoff := time.Now().Add(-time.Duration(r.config.EventRetain) * 24 * time.Hour)
	eventMetadataCutoff := time.Now().Add(-time.Duration(r.eventConfig.RetainDays) * 24 * time.Hour)

	r.cleanSegments(segmentCutoff)
	r.cleanClips(eventCutoff)
	r.cleanSnapshots(eventCutoff)
	r.cleanEventMetadata(eventMetadataCutoff)
	r.cleanMotionActivity(segmentCutoff)
	r.reconcileEventMediaAvailability()
	r.enforceStorageCap()
	r.cleanEmptyDirs()
}

// enforceStorageCap deletes the oldest segments until total storage is under the configured cap.
func (r *Recorder) enforceStorageCap() {
	cap := r.config.MaxStorageBytes()
	if cap <= 0 {
		return
	}

	totalBytes, err := r.db.TotalStorageBytes()
	if err != nil {
		slog.Error("failed to query total storage for cap enforcement", "error", err)
		return
	}

	if totalBytes <= cap {
		return
	}

	slog.Warn("storage exceeds cap, removing oldest segments",
		"total", totalBytes,
		"cap", cap,
	)

	// Fetch oldest segments in batches rather than loading all per camera.
	for totalBytes > cap {
		oldest, err := r.db.GetOldestSegments(20)
		if err != nil {
			slog.Error("failed to query oldest segments", "error", err)
			return
		}
		if len(oldest) == 0 {
			return
		}

		for _, seg := range oldest {
			if totalBytes <= cap {
				return
			}

			slog.Debug("removing segment for storage cap",
				"camera", seg.Camera,
				"path", seg.Path,
			)
			if err := r.segments.RemoveSegment(seg.Camera, seg.Path); err != nil {
				slog.Error("failed to remove segment", "path", seg.Path, "error", err)
				continue
			}
			totalBytes -= seg.SizeBytes
		}
	}
}

func (r *Recorder) cleanSegments(cutoff time.Time) {
	for _, cameraName := range r.listCameras() {
		segments := r.segments.AllSegments(cameraName)
		for _, seg := range segments {
			if seg.EndTime.Before(cutoff) {
				slog.Debug("removing expired segment",
					"camera", cameraName,
					"path", seg.Path,
					"age", time.Since(seg.EndTime).Round(time.Hour),
				)
				if err := r.segments.RemoveSegment(cameraName, seg.Path); err != nil {
					slog.Error("failed to remove segment", "path", seg.Path, "error", err)
				}
			}
		}
	}
}

func (r *Recorder) cleanClips(cutoff time.Time) {
	clipsBase := r.config.Path

	// Walk clips directories and remove files older than the cutoff
	err := filepath.Walk(clipsBase, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip files we can't access
		}
		if info.IsDir() {
			return nil
		}

		// Only clean clip files (not segments)
		if filepath.Ext(path) != ".mp4" {
			return nil
		}

		// Check if this is in a "clips" directory
		dir := filepath.Dir(path)
		if filepath.Base(filepath.Dir(dir)) != "clips" {
			return nil
		}

		if info.ModTime().Before(cutoff) {
			slog.Debug("removing expired clip", "path", path, "age", time.Since(info.ModTime()).Round(time.Hour))
			if err := os.Remove(path); err != nil {
				slog.Warn("failed to remove expired clip", "path", path, "error", err)
			}
		}

		return nil
	})

	if err != nil {
		slog.Error("error walking clips directory", "error", err)
	}
}

func (r *Recorder) cleanSnapshots(cutoff time.Time) {
	if r.snapshotPath == "" {
		return
	}

	err := filepath.Walk(r.snapshotPath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".jpg" {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			slog.Debug("removing expired snapshot", "path", path)
			_ = os.Remove(path)
		}
		return nil
	})
	if err != nil {
		slog.Error("error walking snapshots directory", "error", err)
	}
}

func (r *Recorder) cleanEventMetadata(cutoff time.Time) {
	if err := r.db.DeleteFacesOlderThan(cutoff); err != nil {
		slog.Error("failed to delete expired faces", "error", err)
	}
	if err := r.db.DeleteEventsOlderThan(cutoff); err != nil {
		slog.Error("failed to delete expired events", "error", err)
	}
}

func (r *Recorder) cleanMotionActivity(cutoff time.Time) {
	if err := r.db.DeleteMotionActivityBefore(cutoff); err != nil {
		slog.Error("failed to delete expired motion activity", "error", err)
	}
}

func (r *Recorder) reconcileEventMediaAvailability() {
	events, err := r.db.EventsWithSnapshots()
	if err != nil {
		slog.Error("failed to query events for media reconciliation", "error", err)
		return
	}

	for _, ev := range events {
		snapshotAvailable := ev.SnapshotPath != ""
		if snapshotAvailable {
			if _, err := os.Stat(ev.SnapshotPath); err != nil {
				snapshotAvailable = false
			}
		}
		if err := r.db.UpdateEventSnapshotAvailability(ev.ID, snapshotAvailable); err != nil {
			slog.Error("failed to update snapshot availability", "id", ev.ID, "error", err)
		}

		clipAvailable := ev.ClipPath != ""
		if clipAvailable {
			if _, err := os.Stat(ev.ClipPath); err != nil {
				clipAvailable = false
			}
		}
		if err := r.db.UpdateEventClipAvailability(ev.ID, clipAvailable); err != nil {
			slog.Error("failed to update clip availability", "id", ev.ID, "error", err)
		}
	}
}

// cleanEmptyDirs removes empty directories left after cleanup,
// but preserves structural directories used by active recording (e.g. "segments").
func (r *Recorder) cleanEmptyDirs() {
	r.removeEmptyDirsIn(r.config.Path)
	if r.snapshotPath != "" {
		r.removeEmptyDirsIn(r.snapshotPath)
	}
}

func (r *Recorder) removeEmptyDirsIn(root string) {
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() || path == root {
			return nil
		}

		// Never remove structural directories used by active cameras
		base := filepath.Base(path)
		if base == "segments" || base == "clips" {
			return nil
		}

		entries, err := os.ReadDir(path)
		if err != nil {
			return nil
		}
		if len(entries) == 0 {
			slog.Debug("removing empty directory", "path", path)
			_ = os.Remove(path)
		}

		return nil
	})
}

func (r *Recorder) listCameras() []string {
	var cameras []string
	entries, err := os.ReadDir(r.config.Path)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if entry.IsDir() {
			cameras = append(cameras, entry.Name())
		}
	}
	return cameras
}
