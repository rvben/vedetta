package recording

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/camera"
	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/media"
	"github.com/rvben/vedetta/internal/storage"
)

var errAlwaysFail = errors.New("always fail")

func newTestRecompressor(t *testing.T) (*Recompressor, *storage.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := config.TieredStorageConfig{
		Enabled:      true,
		AfterDays:    1,
		TargetWidth:  1280,
		TargetHeight: 720,
		Schedule:     "02:00-05:00",
	}
	cameras := []config.CameraConfig{
		{Name: "cam1"},
	}
	r := NewRecompressor(cfg, cameras, db, &sync.Mutex{})
	return r, db
}

func TestRecompressionJob_SkipsAlreadyRecompressed(t *testing.T) {
	_, db := newTestRecompressor(t)
	dir := t.TempDir()

	seg := storage.SegmentRecord{
		Camera:    "cam1",
		Path:      filepath.Join(dir, "seg.mp4"),
		StartTime: time.Now().Add(-48 * time.Hour),
		EndTime:   time.Now().Add(-47 * time.Hour),
		SizeBytes: 1000,
	}
	if err := db.SaveSegment(seg); err != nil {
		t.Fatalf("SaveSegment: %v", err)
	}

	// Get the segment ID and mark it as recompressed
	all, err := db.GetAllSegments("cam1")
	if err != nil || len(all) == 0 {
		t.Fatal("expected to find saved segment")
	}
	if err := db.MarkSegmentRecompressed(all[0].ID, 500); err != nil {
		t.Fatalf("MarkSegmentRecompressed: %v", err)
	}

	cutoff := time.Now().Add(-1 * time.Hour)
	segs, err := db.GetSegmentsForRecompression("cam1", cutoff)
	if err != nil {
		t.Fatalf("GetSegmentsForRecompression: %v", err)
	}
	if len(segs) != 0 {
		t.Errorf("expected 0 eligible segments, got %d", len(segs))
	}
}

// saveSegmentForTest stores one eligible segment and returns its assigned ID.
func saveSegmentForTest(t *testing.T, db *storage.DB, path string) int64 {
	t.Helper()
	seg := storage.SegmentRecord{
		Camera:    "cam1",
		Path:      path,
		StartTime: time.Now().Add(-48 * time.Hour),
		EndTime:   time.Now().Add(-47 * time.Hour),
		SizeBytes: 1000,
	}
	if err := db.SaveSegment(seg); err != nil {
		t.Fatalf("SaveSegment: %v", err)
	}
	all, err := db.GetAllSegments("cam1")
	if err != nil || len(all) == 0 {
		t.Fatalf("expected saved segment, err=%v", err)
	}
	return all[0].ID
}

// TestRevalidateSegment_DeletesOrphanRowWhenFileMissing proves a segment whose
// file no longer exists is dropped from the DB and not revalidated, so it stops
// being reselected and failing transcode forever (the failure cap is reset on
// every startup, so a missing file would otherwise retry indefinitely).
func TestRevalidateSegment_DeletesOrphanRowWhenFileMissing(t *testing.T) {
	r, db := newTestRecompressor(t)
	id := saveSegmentForTest(t, db, "/does/not/exist/seg.mp4")
	r.statFn = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }

	target := &recompressTarget{kind: kindSegment, segID: id, camera: "cam1", path: "/does/not/exist/seg.mp4"}
	if r.revalidate(target) {
		t.Fatal("revalidate must return false for a segment whose file is missing")
	}
	seg, err := db.GetSegmentByID(id)
	if err != nil {
		t.Fatalf("GetSegmentByID: %v", err)
	}
	if seg != nil {
		t.Fatal("orphaned segment row was not deleted")
	}
}

// TestRevalidateSegment_KeepsRowOnTransientStatError proves a non-ENOENT stat
// error (e.g. a temporarily unmounted volume) skips the segment WITHOUT deleting
// its metadata, so a transient I/O blip can never destroy recording records.
func TestRevalidateSegment_KeepsRowOnTransientStatError(t *testing.T) {
	r, db := newTestRecompressor(t)
	id := saveSegmentForTest(t, db, "/mnt/ssd/seg.mp4")
	r.statFn = func(string) (os.FileInfo, error) { return nil, errors.New("input/output error") }

	target := &recompressTarget{kind: kindSegment, segID: id, camera: "cam1", path: "/mnt/ssd/seg.mp4"}
	if r.revalidate(target) {
		t.Fatal("revalidate must return false (skip) on a stat error")
	}
	seg, err := db.GetSegmentByID(id)
	if err != nil {
		t.Fatalf("GetSegmentByID: %v", err)
	}
	if seg == nil {
		t.Fatal("segment row was wrongly deleted on a transient (non-ENOENT) stat error")
	}
}

// TestRevalidateSegment_KeepsRowWhenFilePresent proves an eligible segment whose
// file exists revalidates true and is left intact (no regression).
func TestRevalidateSegment_KeepsRowWhenFilePresent(t *testing.T) {
	r, db := newTestRecompressor(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "present.mp4")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := saveSegmentForTest(t, db, path)

	target := &recompressTarget{kind: kindSegment, segID: id, camera: "cam1", path: path}
	if !r.revalidate(target) {
		t.Fatal("revalidate must return true for an eligible segment whose file exists")
	}
	seg, err := db.GetSegmentByID(id)
	if err != nil {
		t.Fatalf("GetSegmentByID: %v", err)
	}
	if seg == nil {
		t.Fatal("present segment row was wrongly deleted")
	}
}

func TestRecompressionJob_RespectsScheduleWindow(t *testing.T) {
	inside := time.Date(2026, 1, 1, 3, 0, 0, 0, time.Local)
	outside := time.Date(2026, 1, 1, 10, 0, 0, 0, time.Local)

	ok, err := config.InScheduleWindow("02:00-05:00", inside)
	if err != nil || !ok {
		t.Errorf("expected inside=true, got %v err=%v", ok, err)
	}
	ok, err = config.InScheduleWindow("02:00-05:00", outside)
	if err != nil || ok {
		t.Errorf("expected outside=false, got %v err=%v", ok, err)
	}
}

func TestRecompressionJob_SkipsDisabledCamera(t *testing.T) {
	_, db := newTestRecompressor(t)

	// Global tiered storage is enabled, but cam_disabled has enabled: false override.
	cfg := config.TieredStorageConfig{Enabled: true, AfterDays: 1, Schedule: "02:00-05:00"}
	disabled := false
	cameras := []config.CameraConfig{
		{Name: "cam_enabled"},
		{Name: "cam_disabled", TieredStorage: config.CameraTieredStorageConfig{Enabled: &disabled}},
	}
	r := NewRecompressor(cfg, cameras, db, &sync.Mutex{})
	eligible := r.eligibleCameras()

	if _, ok := eligible["cam_disabled"]; ok {
		t.Error("cam_disabled should not be eligible when enabled=false")
	}
	if _, ok := eligible["cam_enabled"]; !ok {
		t.Error("cam_enabled should be eligible")
	}
}

func TestRecompressionJob_PerCameraAfterDaysOverride(t *testing.T) {
	_, db := newTestRecompressor(t)
	dir := t.TempDir()
	now := time.Now()

	if err := db.SaveSegment(storage.SegmentRecord{
		Camera: "cam_short", Path: filepath.Join(dir, "a.mp4"),
		StartTime: now.Add(-48 * time.Hour), EndTime: now.Add(-47 * time.Hour), SizeBytes: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveSegment(storage.SegmentRecord{
		Camera: "cam_long", Path: filepath.Join(dir, "b.mp4"),
		StartTime: now.Add(-48 * time.Hour), EndTime: now.Add(-47 * time.Hour), SizeBytes: 1,
	}); err != nil {
		t.Fatal(err)
	}

	global := config.TieredStorageConfig{Enabled: true, AfterDays: 1, Schedule: "02:00-05:00"}
	afterLong := 5
	cameras := []config.CameraConfig{
		{Name: "cam_short"},
		{Name: "cam_long", TieredStorage: config.CameraTieredStorageConfig{AfterDays: &afterLong}},
	}
	r := NewRecompressor(global, cameras, db, &sync.Mutex{})

	segsShort, _ := db.GetSegmentsForRecompression("cam_short", now.Add(-time.Duration(1)*24*time.Hour))
	if len(segsShort) == 0 {
		t.Error("cam_short should have eligible segments")
	}
	segsLong, _ := db.GetSegmentsForRecompression("cam_long", now.Add(-time.Duration(5)*24*time.Hour))
	if len(segsLong) != 0 {
		t.Error("cam_long should have no eligible segments (segment too new)")
	}
	_ = r
}

func TestRecompressionJob_RetriesAfterFailure(t *testing.T) {
	_, db := newTestRecompressor(t)
	dir := t.TempDir()

	seg := storage.SegmentRecord{
		Camera: "cam1", Path: filepath.Join(dir, "seg.mp4"),
		StartTime: time.Now().Add(-48 * time.Hour), EndTime: time.Now().Add(-47 * time.Hour),
		SizeBytes: 1,
	}
	if err := db.SaveSegment(seg); err != nil {
		t.Fatal(err)
	}

	all, _ := db.GetAllSegments("cam1")
	if len(all) == 0 {
		t.Fatal("no segments found")
	}
	id := all[0].ID

	for range 3 {
		if err := db.IncrementSegmentRecompressFailures(id); err != nil {
			t.Fatal(err)
		}
	}

	cutoff := time.Now().Add(-1 * time.Hour)
	segs, err := db.GetSegmentsForRecompression("cam1", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 0 {
		t.Errorf("expected 0 eligible after 3 failures, got %d", len(segs))
	}
}

func TestRecompressionJob_PanicRecovery(t *testing.T) {
	r, db := newTestRecompressor(t)
	dir := t.TempDir()

	segPath := filepath.Join(dir, "panic.mp4")
	if err := os.WriteFile(segPath, make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}

	seg := storage.SegmentRecord{
		Camera: "cam1", Path: segPath,
		StartTime: time.Now().Add(-48 * time.Hour), EndTime: time.Now().Add(-47 * time.Hour),
		SizeBytes: 100,
	}
	if err := db.SaveSegment(seg); err != nil {
		t.Fatal(err)
	}

	// Inject a panicking transcode function to simulate the OpenH264 purego crash
	r.transcodeFn = func(string, int, int) (media.TranscodeResult, error) {
		panic("simulated OpenH264 purego crash")
	}

	// processOne must not panic — it should recover and increment the failure counter
	processed := r.processOne()
	if !processed {
		t.Error("expected processOne to return true (segment was attempted)")
	}

	all, _ := db.GetAllSegments("cam1")
	if len(all) == 0 {
		t.Fatal("no segments found")
	}
	if all[0].RecompressFailures != 1 {
		t.Errorf("expected 1 failure recorded, got %d", all[0].RecompressFailures)
	}
}

func TestRecompressorPicksLargestFirst(t *testing.T) {
	_, db := newTestRecompressor(t)
	dir := t.TempDir()
	now := time.Now()
	old := now.Add(-4 * 24 * time.Hour)

	pathA := filepath.Join(dir, "a.mp4")
	pathB := filepath.Join(dir, "b.mp4")
	pathC := filepath.Join(dir, "c.mp4")
	for _, p := range []string{pathA, pathB, pathC} {
		if err := os.WriteFile(p, make([]byte, 100), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// cam_a: 10MB but oldest. cam_b: 500MB (largest, newer) and 100MB (newest).
	if err := db.SaveSegment(storage.SegmentRecord{
		Camera: "cam_a", Path: pathA,
		StartTime: old, EndTime: old.Add(time.Minute), SizeBytes: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveSegment(storage.SegmentRecord{
		Camera: "cam_b", Path: pathB,
		StartTime: old.Add(time.Hour), EndTime: old.Add(time.Hour).Add(time.Minute), SizeBytes: 500,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveSegment(storage.SegmentRecord{
		Camera: "cam_b", Path: pathC,
		StartTime: old.Add(2 * time.Hour), EndTime: old.Add(2 * time.Hour).Add(time.Minute), SizeBytes: 100,
	}); err != nil {
		t.Fatal(err)
	}

	cfg := config.TieredStorageConfig{
		Enabled: true, AfterDays: 1, Schedule: "00:00-23:59",
		Interval: time.Second, Priority: "largest",
		TargetWidth: 640, TargetHeight: 360,
	}
	cams := []config.CameraConfig{{Name: "cam_a"}, {Name: "cam_b"}}
	r := NewRecompressor(cfg, cams, db, &sync.Mutex{})

	var transcoded []string
	r.transcodeFn = func(path string, w, h int) (media.TranscodeResult, error) {
		transcoded = append(transcoded, path)
		return media.TranscodeResult{OriginalSize: 500, NewSize: 50}, nil
	}

	if !r.processOne() {
		t.Fatal("processOne returned false, want true")
	}
	if len(transcoded) != 1 || transcoded[0] != pathB {
		t.Fatalf("first transcode = %v, want [%s]", transcoded, pathB)
	}
}

func TestRecompressorPicksOldestWhenConfigured(t *testing.T) {
	_, db := newTestRecompressor(t)
	dir := t.TempDir()
	now := time.Now()
	old := now.Add(-4 * 24 * time.Hour)

	pathA := filepath.Join(dir, "a.mp4")
	pathB := filepath.Join(dir, "b.mp4")
	for _, p := range []string{pathA, pathB} {
		if err := os.WriteFile(p, make([]byte, 100), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// cam_a: 10MB, oldest. cam_b: 500MB, newer. With Priority=oldest, /a.mp4 wins.
	if err := db.SaveSegment(storage.SegmentRecord{
		Camera: "cam_a", Path: pathA,
		StartTime: old, EndTime: old.Add(time.Minute), SizeBytes: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveSegment(storage.SegmentRecord{
		Camera: "cam_b", Path: pathB,
		StartTime: old.Add(time.Hour), EndTime: old.Add(time.Hour).Add(time.Minute), SizeBytes: 500,
	}); err != nil {
		t.Fatal(err)
	}

	cfg := config.TieredStorageConfig{
		Enabled: true, AfterDays: 1, Schedule: "00:00-23:59",
		Interval: time.Second, Priority: "oldest",
		TargetWidth: 640, TargetHeight: 360,
	}
	cams := []config.CameraConfig{{Name: "cam_a"}, {Name: "cam_b"}}
	r := NewRecompressor(cfg, cams, db, &sync.Mutex{})

	var transcoded []string
	r.transcodeFn = func(path string, w, h int) (media.TranscodeResult, error) {
		transcoded = append(transcoded, path)
		return media.TranscodeResult{OriginalSize: 10, NewSize: 5}, nil
	}

	if !r.processOne() {
		t.Fatal("processOne returned false, want true")
	}
	if len(transcoded) != 1 || transcoded[0] != pathA {
		t.Fatalf("first transcode = %v, want [%s]", transcoded, pathA)
	}
}

func TestRecompressionJob_UpdatesDBOnSuccess(t *testing.T) {
	_, db := newTestRecompressor(t)
	dir := t.TempDir()

	f, _ := os.Create(filepath.Join(dir, "seg.mp4"))
	_, _ = f.Write(make([]byte, 1000))
	f.Close()

	seg := storage.SegmentRecord{
		Camera: "cam1", Path: filepath.Join(dir, "seg.mp4"),
		StartTime: time.Now().Add(-48 * time.Hour), EndTime: time.Now().Add(-47 * time.Hour),
		SizeBytes: 1000,
	}
	if err := db.SaveSegment(seg); err != nil {
		t.Fatal(err)
	}
	all, _ := db.GetAllSegments("cam1")
	id := all[0].ID

	if err := db.MarkSegmentRecompressed(id, 500); err != nil {
		t.Fatalf("MarkSegmentRecompressed: %v", err)
	}

	updated, _ := db.GetSegmentByID(id)
	if !updated.Recompressed {
		t.Error("expected recompressed=true")
	}
	if updated.SizeBytes != 500 {
		t.Errorf("size_bytes = %d, want 500", updated.SizeBytes)
	}
	if updated.RecompressedAt.IsZero() {
		t.Error("expected recompressed_at to be set")
	}
}

// seedClip writes a real clip file and an event row that references it, making
// it an eligible recompression candidate (ended `age` ago).
func seedClip(t *testing.T, db *storage.DB, dir, id, cam string, age time.Duration, size int) string {
	t.Helper()
	path := filepath.Join(dir, id+".mp4")
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	end := time.Now().Add(-age)
	ev := camera.Event{ID: id, CameraName: cam, Label: "person", Score: 0.9, Box: [4]int{1, 2, 3, 4}, Timestamp: end.Add(-time.Minute), EndTime: end}
	if err := db.SaveEvent(ev); err != nil {
		t.Fatalf("SaveEvent: %v", err)
	}
	if err := db.SetEventClip(id, path, int64(size)); err != nil {
		t.Fatalf("SetEventClip: %v", err)
	}
	return path
}

func TestRecompressor_PicksLargestClipOverSmallerSegment(t *testing.T) {
	_, db := newTestRecompressor(t)
	dir := t.TempDir()

	segPath := filepath.Join(dir, "seg.mp4")
	if err := os.WriteFile(segPath, make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveSegment(storage.SegmentRecord{
		Camera: "cam1", Path: segPath,
		StartTime: time.Now().Add(-48 * time.Hour), EndTime: time.Now().Add(-47 * time.Hour),
		SizeBytes: 100,
	}); err != nil {
		t.Fatal(err)
	}
	clipPath := seedClip(t, db, dir, "clipA", "cam1", 47*time.Hour, 500)

	cfg := config.TieredStorageConfig{
		Enabled: true, AfterDays: 1, Schedule: "00:00-23:59",
		Interval: time.Second, Priority: "largest", TargetWidth: 640, TargetHeight: 360,
	}
	r := NewRecompressor(cfg, []config.CameraConfig{{Name: "cam1"}}, db, &sync.Mutex{})

	var transcoded []string
	r.transcodeFn = func(path string, w, h int) (media.TranscodeResult, error) {
		transcoded = append(transcoded, path)
		return media.TranscodeResult{OriginalSize: 500, NewSize: 50}, nil
	}

	if !r.processOne() {
		t.Fatal("processOne returned false, want true")
	}
	if len(transcoded) != 1 || transcoded[0] != clipPath {
		t.Fatalf("transcoded = %v, want [%s]", transcoded, clipPath)
	}
	st, _, _ := db.GetClipRecompressState("clipA")
	if !st.Recompressed {
		t.Error("expected clipA recompressed=true")
	}
}

func TestRecompressor_ClipTranscodeFailureIncrementsClipFailures(t *testing.T) {
	_, db := newTestRecompressor(t)
	dir := t.TempDir()
	seedClip(t, db, dir, "clipF", "cam1", 47*time.Hour, 500)

	cfg := config.TieredStorageConfig{
		Enabled: true, AfterDays: 1, Schedule: "00:00-23:59",
		Interval: time.Second, Priority: "largest", TargetWidth: 640, TargetHeight: 360,
	}
	r := NewRecompressor(cfg, []config.CameraConfig{{Name: "cam1"}}, db, &sync.Mutex{})
	r.transcodeFn = func(string, int, int) (media.TranscodeResult, error) {
		return media.TranscodeResult{}, errAlwaysFail
	}

	if !r.processOne() {
		t.Fatal("processOne returned false, want true")
	}
	clips, _ := db.GetClipsForRecompression("cam1", time.Now().Add(-time.Hour))
	if len(clips) != 1 || clips[0].RecompressFailures != 1 {
		t.Fatalf("clip failures not incremented: %+v", clips)
	}
	st, _, _ := db.GetClipRecompressState("clipF")
	if st.Recompressed {
		t.Error("failed transcode must leave recompressed=false")
	}
}

func TestRecompressor_RecordsWorkerTimeout(t *testing.T) {
	r, db := newTestRecompressor(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "segment.mp4")
	if err := os.WriteFile(path, []byte("recording"), 0o600); err != nil {
		t.Fatal(err)
	}
	saveSegmentForTest(t, db, path)
	r.transcodeFn = func(string, int, int) (media.TranscodeResult, error) {
		return media.TranscodeResult{}, &transcodeWorkerError{
			kind: transcodeFailureTimeout,
			err:  errors.New("worker exceeded deadline"),
		}
	}

	if !r.processOne() {
		t.Fatal("processOne returned false, want a handled failure")
	}
	stats := r.Stats()
	if stats.TranscodeFailures != 1 || stats.WorkerTimeouts != 1 || stats.WorkerCrashes != 0 {
		t.Fatalf("failure stats = %+v, want one timeout and no crash", stats)
	}
}

func TestRecompressor_RevalidateSkipsMissingClipFile(t *testing.T) {
	_, db := newTestRecompressor(t)
	dir := t.TempDir()
	clipPath := seedClip(t, db, dir, "clipG", "cam1", 47*time.Hour, 500)

	cfg := config.TieredStorageConfig{
		Enabled: true, AfterDays: 1, Schedule: "00:00-23:59",
		Interval: time.Second, Priority: "oldest", TargetWidth: 640, TargetHeight: 360,
	}
	r := NewRecompressor(cfg, []config.CameraConfig{{Name: "cam1"}}, db, &sync.Mutex{})

	transcodeCalled := false
	r.transcodeFn = func(string, int, int) (media.TranscodeResult, error) {
		transcodeCalled = true
		return media.TranscodeResult{OriginalSize: 500, NewSize: 50}, nil
	}

	if err := os.Remove(clipPath); err != nil {
		t.Fatal(err)
	}

	if !r.processOne() {
		t.Fatal("processOne returned false, want true (it did the skip decision)")
	}
	if transcodeCalled {
		t.Error("must not transcode a missing clip file")
	}
	st, _, _ := db.GetClipRecompressState("clipG")
	if st.ClipAvailable {
		t.Error("missing clip file must be reconciled to unavailable")
	}
	if st.Recompressed {
		t.Error("missing clip must not be marked recompressed")
	}
}

func TestRecompressor_RunNowDrainsSegmentsAndClips(t *testing.T) {
	_, db := newTestRecompressor(t)
	dir := t.TempDir()

	segPath := filepath.Join(dir, "seg.mp4")
	if err := os.WriteFile(segPath, make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveSegment(storage.SegmentRecord{
		Camera: "cam1", Path: segPath,
		StartTime: time.Now().Add(-48 * time.Hour), EndTime: time.Now().Add(-47 * time.Hour),
		SizeBytes: 100,
	}); err != nil {
		t.Fatal(err)
	}
	seedClip(t, db, dir, "c1", "cam1", 47*time.Hour, 300)
	seedClip(t, db, dir, "c2", "cam1", 46*time.Hour, 200)

	cfg := config.TieredStorageConfig{
		Enabled: true, AfterDays: 1, Schedule: "00:00-23:59",
		Interval: time.Second, Priority: "largest", TargetWidth: 640, TargetHeight: 360,
	}
	r := NewRecompressor(cfg, []config.CameraConfig{{Name: "cam1"}}, db, &sync.Mutex{})
	r.transcodeFn = func(string, int, int) (media.TranscodeResult, error) {
		return media.TranscodeResult{OriginalSize: 300, NewSize: 30}, nil
	}

	r.RunNow(context.Background())

	stats := r.Stats()
	if stats.SegmentsRecompressed != 1 {
		t.Errorf("SegmentsRecompressed = %d, want 1", stats.SegmentsRecompressed)
	}
	if stats.ClipsRecompressed != 2 {
		t.Errorf("ClipsRecompressed = %d, want 2", stats.ClipsRecompressed)
	}
	if r.processOne() {
		t.Error("processOne should return false after draining")
	}
}

// TestRecompressor_LargestInUseSkipsToSmallerSegment verifies that when the
// largest eligible segment is HLS-in-use, processOne picks the next-largest
// non-in-use segment rather than skipping the camera entirely.
func TestRecompressor_LargestInUseSkipsToSmallerSegment(t *testing.T) {
	_, db := newTestRecompressor(t)
	dir := t.TempDir()

	bigPath := filepath.Join(dir, "big.mp4")
	smallPath := filepath.Join(dir, "small.mp4")
	if err := os.WriteFile(bigPath, make([]byte, 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(smallPath, make([]byte, 200), 0o644); err != nil {
		t.Fatal(err)
	}

	// Both segments are old enough to be eligible (ended 48h ago).
	if err := db.SaveSegment(storage.SegmentRecord{
		Camera: "cam1", Path: bigPath,
		StartTime: time.Now().Add(-49 * time.Hour), EndTime: time.Now().Add(-48 * time.Hour),
		SizeBytes: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveSegment(storage.SegmentRecord{
		Camera: "cam1", Path: smallPath,
		StartTime: time.Now().Add(-49 * time.Hour), EndTime: time.Now().Add(-48 * time.Hour),
		SizeBytes: 200,
	}); err != nil {
		t.Fatal(err)
	}

	cfg := config.TieredStorageConfig{
		Enabled: true, AfterDays: 1, Schedule: "00:00-23:59",
		Interval: time.Second, Priority: "largest", TargetWidth: 640, TargetHeight: 360,
	}
	r := NewRecompressor(cfg, []config.CameraConfig{{Name: "cam1"}}, db, &sync.Mutex{})

	// Report bigPath as HLS-in-use so processOne must skip it.
	r.inUseFn = func(p string) bool { return p == bigPath }

	var transcodedPath string
	r.transcodeFn = func(path string, w, h int) (media.TranscodeResult, error) {
		transcodedPath = path
		return media.TranscodeResult{OriginalSize: 200, NewSize: 10}, nil
	}

	if !r.processOne() {
		t.Fatal("processOne returned false, want true (smallPath is eligible and not in-use)")
	}
	if transcodedPath != smallPath {
		t.Fatalf("transcoded %q, want %q (the smaller non-in-use segment)", transcodedPath, smallPath)
	}
}

func TestRecompressKind_String(t *testing.T) {
	if got := kindSegment.String(); got != "segment" {
		t.Errorf("kindSegment.String() = %q, want \"segment\"", got)
	}
	if got := kindClip.String(); got != "clip" {
		t.Errorf("kindClip.String() = %q, want \"clip\"", got)
	}
	if got := recompressKind(99).String(); got != "unknown" {
		t.Errorf("recompressKind(99).String() = %q, want \"unknown\"", got)
	}
}

func TestRecompressionStats_HasClipsField(t *testing.T) {
	// The recording.RecompressionStats snapshot must expose ClipsRecompressed
	// so the API layer can surface it.
	var s RecompressionStats
	s.ClipsRecompressed = 7
	if s.ClipsRecompressed != 7 {
		t.Fatalf("ClipsRecompressed = %d, want 7", s.ClipsRecompressed)
	}
}

func TestRecompressor_ConcurrentWithCleanup(t *testing.T) {
	_, db := newTestRecompressor(t)
	dir := t.TempDir()
	lock := &sync.Mutex{}

	for i := 0; i < 20; i++ {
		seedClip(t, db, dir, fmt.Sprintf("c%d", i), "cam1", time.Duration(40+i)*time.Hour, 100+i)
	}

	cfg := config.TieredStorageConfig{
		Enabled: true, AfterDays: 1, Schedule: "00:00-23:59",
		Interval: time.Second, Priority: "largest", TargetWidth: 640, TargetHeight: 360,
	}
	r := NewRecompressor(cfg, []config.CameraConfig{{Name: "cam1"}}, db, lock)
	r.transcodeFn = func(string, int, int) (media.TranscodeResult, error) {
		return media.TranscodeResult{OriginalSize: 100, NewSize: 10}, nil
	}

	var wg sync.WaitGroup
	wg.Add(3)

	// Recompressor.
	go func() {
		defer wg.Done()
		for i := 0; i < 30; i++ {
			r.processOne()
		}
	}()
	// Backfill batches (take the lock like Start would).
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			lock.Lock()
			_, _ = db.BackfillClipSizes(5)
			lock.Unlock()
		}
	}()
	// Simulated cleanup clearing clip refs under the lock.
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			lock.Lock()
			_ = db.ClearEventClip(fmt.Sprintf("c%d", i))
			lock.Unlock()
		}
	}()

	wg.Wait()

	// State must be consistent: processOne drains without panicking or racing.
	for i := 0; i < 30; i++ {
		if !r.processOne() {
			break
		}
	}
}

func TestRecompressor_PreservesClipModTimeAcrossRecompression(t *testing.T) {
	_, db := newTestRecompressor(t)
	dir := t.TempDir()

	clipPath := seedClip(t, db, dir, "clipM", "cam1", 47*time.Hour, 500)

	// Set a known old mtime so the retention-by-mtime logic treats this clip as
	// nearly expiring. After recompression the mtime must be restored to this
	// value, not bumped to now.
	oldMod := time.Now().Add(-47 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(clipPath, oldMod, oldMod); err != nil {
		t.Fatal(err)
	}

	cfg := config.TieredStorageConfig{
		Enabled: true, AfterDays: 1, Schedule: "00:00-23:59",
		Interval: time.Second, Priority: "largest", TargetWidth: 640, TargetHeight: 360,
	}
	r := NewRecompressor(cfg, []config.CameraConfig{{Name: "cam1"}}, db, &sync.Mutex{})

	// Fake transcode rewrites the file (mimicking the real temp+rename mtime bump)
	// and returns a smaller size.
	r.transcodeFn = func(path string, w, h int) (media.TranscodeResult, error) {
		if err := os.WriteFile(path, make([]byte, 50), 0o644); err != nil {
			return media.TranscodeResult{}, err
		}
		return media.TranscodeResult{OriginalSize: 500, NewSize: 50}, nil
	}

	if !r.processOne() {
		t.Fatal("processOne returned false, want true")
	}

	fi, err := os.Stat(clipPath)
	if err != nil {
		t.Fatal(err)
	}
	if d := fi.ModTime().Sub(oldMod); d > 2*time.Second || d < -2*time.Second {
		t.Errorf("clip mtime not preserved: got %v, want ~%v (diff %v)", fi.ModTime(), oldMod, d)
	}
}
