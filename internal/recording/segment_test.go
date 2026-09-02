package recording

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/media"
	"github.com/rvben/vedetta/internal/rtsp"
	"github.com/rvben/vedetta/internal/storage"
)

func newTestSegmentRecorder(t *testing.T) *SegmentRecorder {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := storage.New(dbPath)
	if err != nil {
		t.Fatalf("failed to create test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewSegmentRecorder(config.RecordingConfig{}, db, nil)
}

func seedSegments(t *testing.T, sr *SegmentRecorder, segments []storage.SegmentRecord) {
	t.Helper()
	for _, seg := range segments {
		if err := sr.db.SaveSegment(seg); err != nil {
			t.Fatalf("failed to seed segment: %v", err)
		}
	}
}

// A transient os.MkdirAll failure on the recording volume (the mount briefly
// unavailable just after a reboot/USB stall) must NOT permanently abandon a
// camera's recording. Production observed: front_door's StartRecording hit
// "mkdir /Volumes/VedettaSSD: permission denied" during the post-reboot window,
// returned, and never recorded again for the process lifetime - detect stream
// healthy, zero segments, every event clip failing "no segments available".
// Recording must self-heal once the volume returns, like RTSP reconnect and
// the consumer's disk-full pause/resume already do.
func TestSegmentRecorder_TransientSegmentDirFailure_SelfHeals(t *testing.T) {
	base := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := storage.New(dbPath)
	if err != nil {
		t.Fatalf("create db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := rtsp.NewHub(ctx)

	sr := NewSegmentRecorder(config.RecordingConfig{Path: base}, db, hub)

	// Obstruct: a regular file where the camera's directory must go, so
	// os.MkdirAll(<base>/front_door/segments) fails (ENOTDIR) - the transient
	// "volume not ready" condition, deterministically.
	obstruction := filepath.Join(sr.baseDir, "front_door")
	if err := os.WriteFile(obstruction, []byte("x"), 0o644); err != nil {
		t.Fatalf("write obstruction: %v", err)
	}

	sr.StartRecording(ctx, "front_door", "rtsp://127.0.0.1:9/none")

	// The camera must be under management (a cancelable retry exists), not
	// silently abandoned. Today StartRecording returns before registering
	// anything, so this is empty - the bug.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sr.mu.Lock()
		_, managed := sr.sessions["front_door"]
		sr.mu.Unlock()
		if managed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	sr.mu.Lock()
	_, managed := sr.sessions["front_door"]
	sr.mu.Unlock()
	if !managed {
		t.Fatal("front_door was permanently abandoned on a transient segment-dir failure: no managed retry, recording bricked until full process restart")
	}

	// Volume returns: clear the obstruction. Recording must self-heal and
	// create the segment directory without any external restart.
	if err := os.Remove(obstruction); err != nil {
		t.Fatalf("remove obstruction: %v", err)
	}
	segDir := filepath.Join(sr.baseDir, "front_door", "segments")
	healDeadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(healDeadline) {
		if fi, err := os.Stat(segDir); err == nil && fi.IsDir() {
			return // self-healed: recording recovered on its own
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("segment directory never created after the transient failure cleared: recording did not self-heal")
}

func TestSegmentRecorder_FindSegments(t *testing.T) {
	sr := newTestSegmentRecorder(t)
	now := time.Now().Truncate(time.Second)

	seedSegments(t, sr, []storage.SegmentRecord{
		{Camera: "cam1", Path: "/seg1.mp4", StartTime: now.Add(-30 * time.Minute), EndTime: now.Add(-20 * time.Minute)},
		{Camera: "cam1", Path: "/seg2.mp4", StartTime: now.Add(-20 * time.Minute), EndTime: now.Add(-10 * time.Minute)},
		{Camera: "cam1", Path: "/seg3.mp4", StartTime: now.Add(-10 * time.Minute), EndTime: now},
	})

	from := now.Add(-15 * time.Minute)
	to := now.Add(-5 * time.Minute)
	result := sr.FindSegments("cam1", from, to)

	if len(result) != 2 {
		t.Errorf("expected 2 overlapping segments, got %d", len(result))
	}
}

func TestSegmentRecorder_FindSegments_NoMatch(t *testing.T) {
	sr := newTestSegmentRecorder(t)
	now := time.Now().Truncate(time.Second)

	seedSegments(t, sr, []storage.SegmentRecord{
		{Camera: "cam1", Path: "/seg1.mp4", StartTime: now.Add(-30 * time.Minute), EndTime: now.Add(-20 * time.Minute)},
	})

	from := now.Add(-5 * time.Minute)
	to := now
	result := sr.FindSegments("cam1", from, to)

	if len(result) != 0 {
		t.Errorf("expected 0 segments, got %d", len(result))
	}
}

func TestSegmentRecorder_FindSegments_UnknownCamera(t *testing.T) {
	sr := newTestSegmentRecorder(t)

	result := sr.FindSegments("nonexistent", time.Now().Add(-time.Hour), time.Now())

	if len(result) != 0 {
		t.Errorf("expected 0 segments for unknown camera, got %d", len(result))
	}
}

func TestCurrentSegmentPaths(t *testing.T) {
	sr := &SegmentRecorder{}
	if got := sr.CurrentSegmentPaths(); len(got) != 0 {
		t.Errorf("empty SegmentRecorder = %v, want empty slice", got)
	}

	rc1 := &media.RecordingConsumer{}
	sr.consumers = []*media.RecordingConsumer{rc1}
	_ = sr.CurrentSegmentPaths() // smoke: should not panic, returns []
}

func TestSegmentRecorder_RemoveSegment(t *testing.T) {
	sr := newTestSegmentRecorder(t)
	now := time.Now().Truncate(time.Second)

	seedSegments(t, sr, []storage.SegmentRecord{
		{Camera: "cam1", Path: "/seg1.mp4", StartTime: now.Add(-20 * time.Minute), EndTime: now.Add(-10 * time.Minute)},
		{Camera: "cam1", Path: "/seg2.mp4", StartTime: now.Add(-10 * time.Minute), EndTime: now},
	})

	_ = sr.RemoveSegment("cam1", "/seg1.mp4")

	segs := sr.AllSegments("cam1")
	if len(segs) != 1 {
		t.Errorf("expected 1 segment after removal, got %d", len(segs))
	}
	if segs[0].Path != "/seg2.mp4" {
		t.Errorf("expected remaining segment /seg2.mp4, got %s", segs[0].Path)
	}
}
