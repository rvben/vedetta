package media

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/rtsp"
)

// breakWriterFile swaps the writer's file for the write end of a pipe whose
// read end is closed. Writes then fail with EPIPE while Close still succeeds,
// which isolates a failed final flush from a failed file close. Returns the
// original file so the caller can release it.
func breakWriterFile(t *testing.T, sw *SegmentWriter) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close pipe read end: %v", err)
	}
	sw.mu.Lock()
	orig := sw.f
	sw.f = w
	sw.mu.Unlock()
	t.Cleanup(func() { _ = orig.Close() })
}

// The last GOP of a segment is only written during Close, so a failure there
// loses every frame since the last keyframe. Close discarded the flush error
// and reported success, and the recording consumer then registered the
// truncated file as a healthy segment covering the full media duration.
func TestSegmentWriter_CloseReportsFlushFailure(t *testing.T) {
	sw := newTestVideoWriter(t, "flushfail.mp4")

	if err := sw.WriteVideo(h264TestPacket(1, 0, 0x65)); err != nil {
		t.Fatalf("write keyframe: %v", err)
	}
	for i := uint16(2); i <= 4; i++ {
		if err := sw.WriteVideo(h264TestPacket(i, uint32(i-1)*3000, 0x41)); err != nil {
			t.Fatalf("write frame %d: %v", i, err)
		}
	}

	breakWriterFile(t, sw)

	_, err := sw.Close()
	if err == nil {
		t.Fatal("Close reported success while the final GOP failed to reach disk: the caller then registers a truncated segment as complete")
	}
}

// A segment whose final flush failed must not report the media duration of
// samples that never reached the file.
func TestSegmentWriter_FlushedDurationExcludesLostGOP(t *testing.T) {
	sw := newTestVideoWriter(t, "flushed.mp4")

	// First GOP: 3 frames, flushed when the second keyframe arrives.
	if err := sw.WriteVideo(h264TestPacket(1, 0, 0x65)); err != nil {
		t.Fatalf("write keyframe: %v", err)
	}
	if err := sw.WriteVideo(h264TestPacket(2, 3000, 0x41)); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	if err := sw.WriteVideo(h264TestPacket(3, 6000, 0x41)); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	// Second GOP starts here, flushing the first (3 samples x 3000 ticks).
	if err := sw.WriteVideo(h264TestPacket(4, 9000, 0x65)); err != nil {
		t.Fatalf("write keyframe 2: %v", err)
	}
	if err := sw.WriteVideo(h264TestPacket(5, 12000, 0x41)); err != nil {
		t.Fatalf("write frame: %v", err)
	}

	breakWriterFile(t, sw)
	_, _ = sw.Close()

	want := 100 * time.Millisecond // 3 samples x 3000 ticks at 90kHz
	if got := sw.FlushedDuration(); got != want {
		t.Fatalf("FlushedDuration() = %v, want %v (only the first GOP reached disk)", got, want)
	}
}

// A stream that stops emitting keyframes must not buffer the whole segment in
// memory: parts are flushed on a duration and byte ceiling too. Non-keyframe
// fMP4 parts are legal, and the segment reader keys seeks on sample flags
// rather than fragment boundaries, so the extra parts stay seekable.
func TestSegmentWriter_LongGOPFlushesWithoutKeyframe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nokeyframe.mp4")
	video := &rtsp.TrackInfo{
		Codec: "H264", ClockRate: 90000, IsVideo: true,
		SPS: []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2},
		PPS: []byte{0x68, 0xce, 0x38, 0x80},
	}
	sw, err := NewSegmentWriter(path, video, nil)
	if err != nil {
		t.Fatalf("NewSegmentWriter: %v", err)
	}

	// 10 seconds of 30fps video, one keyframe and no further keyframe. The
	// access-unit decoder emits one frame behind, so the init segment appears
	// after the second packet.
	const frames = 300
	var initSize int64
	if err := sw.WriteVideo(h264TestPacket(1, 0, 0x65)); err != nil {
		t.Fatalf("write keyframe: %v", err)
	}
	for i := 1; i <= frames; i++ {
		if err := sw.WriteVideo(h264TestPacket(uint16(i+1), uint32(i)*3000, 0x41)); err != nil {
			t.Fatalf("write frame %d: %v", i, err)
		}
		if i == 1 {
			initSize = fileSize(t, path)
			if initSize == 0 {
				t.Fatal("init segment was not written")
			}
		}
	}

	if got := fileSize(t, path); got == initSize {
		t.Fatalf("no media written after %d frames without a keyframe: the whole segment is buffered in memory (file still %d bytes, the init segment alone)", frames, got)
	}
	sw.mu.Lock()
	pending := len(sw.pendingVideoSamples)
	sw.mu.Unlock()
	if pending >= frames {
		t.Fatalf("%d samples still buffered after %d frames: pending GOP is unbounded", pending, frames)
	}

	if _, err := sw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Splitting the GOP must not corrupt timing: the file still probes to the
	// full media duration it was fed.
	dur, err := ProbeDuration(path)
	if err != nil {
		t.Fatalf("ProbeDuration on a split-GOP segment: %v", err)
	}
	want := time.Duration(frames+1) * 3000 * time.Second / 90000
	if diff := dur - want; diff > 50*time.Millisecond || diff < -50*time.Millisecond {
		t.Fatalf("ProbeDuration = %v, want ~%v", dur, want)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}
