package media

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/rvben/vedetta/internal/rtsp"
)

func testDisk(t *testing.T) *DiskSpace {
	return NewDiskSpace(t.TempDir())
}

// A stalled storage volume makes os.Create block forever in the kernel.
// Segment creation runs in the single processLoop goroutine under rc.mu, so
// an unbounded hang there wedges recording permanently (observed in
// production: 0 segments, 0 open file handles, recorder "started" but never
// writing). Creation must be bounded and converted into the existing
// pause/resume recovery path instead.
func TestRecordingConsumer_StalledSegmentCreate_PausesNotWedges(t *testing.T) {
	dir := t.TempDir()
	video := &rtsp.TrackInfo{
		Codec:     "H264",
		ClockRate: 90000,
		IsVideo:   true,
		SPS:       []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2},
		PPS:       []byte{0x68, 0xce, 0x38, 0x80},
	}

	rc := NewRecordingConsumer(dir, "test-cam", time.Minute, video, nil, testDisk(t), nil)
	defer rc.Close()

	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	// Set before any packet is sent: processLoop is parked on the packet
	// channel and only reads newWriter after receiving a packet, so the
	// channel send/receive establishes happens-before (no data race).
	rc.newWriter = func(string, *rtsp.TrackInfo, *rtsp.TrackInfo) (*SegmentWriter, error) {
		<-block // simulate a stalled volume: creation never returns
		return nil, nil
	}

	for i := 0; i < 5; i++ {
		rc.OnVideoRTP(&rtp.Packet{
			Header:  rtp.Header{PayloadType: 96, Timestamp: uint32(i * 3000)},
			Payload: []byte{0x65, 0x00, 0x01},
		})
	}

	deadline := time.Now().Add(3*segmentWriterCreateTimeout + 2*time.Second)
	for time.Now().Before(deadline) {
		if rc.Paused() {
			return // recovered into the pause path: correct
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("recording wedged on a stalled segment create: never paused, processLoop is blocked forever under rc.mu")
}

func TestRecordingConsumer_SegmentCallback(t *testing.T) {
	dir := t.TempDir()

	video := &rtsp.TrackInfo{
		Codec:     "H264",
		ClockRate: 90000,
		IsVideo:   true,
		SPS:       []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2},
		PPS:       []byte{0x68, 0xce, 0x38, 0x80},
	}

	var mu sync.Mutex
	var segments []SegmentInfo

	rc := NewRecordingConsumer(dir, "test-cam", time.Second, video, nil, testDisk(t), func(info SegmentInfo) {
		mu.Lock()
		segments = append(segments, info)
		mu.Unlock()
	})

	// Close immediately triggers segment callback if data was written
	rc.Close()

	// No packets were written, so we may or may not get a callback
	// depending on whether the writer was initialized
	mu.Lock()
	defer mu.Unlock()
	// This is valid — no crash, no panic
}

// A mid-stream RTP timestamp gap (camera stall, Wi-Fi dropout without an RTSP
// disconnect) must rotate to a new segment file instead of compressing the gap
// into the current one. Otherwise every wall-time seek that lands after the
// gap inside the same 10-minute file starts playback minutes off target, and
// the timeline shows coverage that does not exist.
func TestRecordingConsumer_TimestampGapRotatesSegment(t *testing.T) {
	dir := t.TempDir()
	video := &rtsp.TrackInfo{
		Codec: "H264", ClockRate: 90000, IsVideo: true,
		SPS: []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2},
		PPS: []byte{0x68, 0xce, 0x38, 0x80},
	}

	var mu sync.Mutex
	latest := map[string]SegmentInfo{} // upsert-by-path, like the DB
	rc := NewRecordingConsumer(dir, "test-cam", time.Minute, video, nil, testDisk(t), func(info SegmentInfo) {
		mu.Lock()
		latest[info.Path] = info
		mu.Unlock()
	})

	// 3 frames at 30fps, then 60 seconds of missing stream, then 3 more
	// frames resuming on a keyframe (cameras emit one after a stall).
	feed := func(seq uint16, ts uint32, nal byte) {
		rc.OnVideoRTP(h264TestPacket(seq, ts, nal))
	}
	feed(1, 0, 0x65)
	feed(2, 3000, 0x41)
	feed(3, 6000, 0x41)
	resumeTS := uint32(6000 + 60*90000)
	feed(4, resumeTS, 0x65)
	feed(5, resumeTS+3000, 0x41)
	feed(6, resumeTS+6000, 0x41)

	rc.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(latest) != 2 {
		paths := make([]string, 0, len(latest))
		for p := range latest {
			paths = append(paths, p)
		}
		t.Fatalf("got %d segment files %v, want 2 (gap must rotate)", len(latest), paths)
	}
	for path, info := range latest {
		// Each file holds ~100ms of media; a duration anywhere near the 60s
		// gap means the gap leaked into a file.
		if d := info.EndTime.Sub(info.StartTime); d > 5*time.Second {
			t.Errorf("segment %s spans %v, want ~100ms (gap compressed into file)", path, d)
		}
	}
}

func TestRecordingConsumer_Close_NilWriter(t *testing.T) {
	dir := t.TempDir()

	rc := NewRecordingConsumer(dir, "test-cam", time.Minute, nil, nil, testDisk(t), nil)
	rc.Close() // should not panic
}

func TestRecordingConsumer_OnDisconnect_ClosesSegment(t *testing.T) {
	dir := t.TempDir()

	var mu sync.Mutex
	var segments []SegmentInfo

	rc := NewRecordingConsumer(dir, "test-cam", time.Minute, nil, nil, testDisk(t), func(info SegmentInfo) {
		mu.Lock()
		segments = append(segments, info)
		mu.Unlock()
	})

	rc.OnDisconnect()
	rc.Close()
	// Should handle multiple close/disconnect calls gracefully
}

func TestRecordingConsumer_SegmentDir_Created(t *testing.T) {
	base := t.TempDir()
	segDir := filepath.Join(base, "nested", "segments")

	rc := NewRecordingConsumer(segDir, "test-cam", time.Minute, nil, nil, testDisk(t), nil)
	rc.Close()

	if _, err := os.Stat(segDir); os.IsNotExist(err) {
		t.Error("segment directory was not created")
	}
}

func TestRecordingConsumer_PausedState(t *testing.T) {
	dir := t.TempDir()
	disk := testDisk(t)

	rc := NewRecordingConsumer(dir, "test-cam", time.Minute, nil, nil, disk, nil)
	defer rc.Close()

	if rc.Paused() {
		t.Error("consumer should not be paused on start with available disk space")
	}
}

func TestRecordingConsumer_Accessors(t *testing.T) {
	rc := &RecordingConsumer{camera: "kids_bedroom_3"}
	if got := rc.Camera(); got != "kids_bedroom_3" {
		t.Errorf("Camera() = %q, want kids_bedroom_3", got)
	}
	if got := rc.CurrentSegmentPath(); got != "" {
		t.Errorf("CurrentSegmentPath() with no open segment = %q, want empty", got)
	}

	// Simulate an open segment by setting the writer path field directly.
	rc.mu.Lock()
	rc.currentPath = "/tmp/seg-001.mp4"
	rc.mu.Unlock()

	if got := rc.CurrentSegmentPath(); got != "/tmp/seg-001.mp4" {
		t.Errorf("CurrentSegmentPath() = %q, want /tmp/seg-001.mp4", got)
	}
}

// TestRecordingConsumer_PanicDuringDecodeKeepsConsumerAlive proves that a
// panic while processing one packet (the malformed-input class that wedged
// production: a corrupt/garbage packet making the H264 decode path panic) is
// recovered per-packet, so the processLoop goroutine survives and keeps
// draining the camera instead of taking the whole process down. A nil packet
// reaches h264Decoder.Decode(nil) inside WriteVideo and panics with a nil
// pointer dereference - a real code path, no mocks.
func TestRecordingConsumer_PanicDuringDecodeKeepsConsumerAlive(t *testing.T) {
	dir := t.TempDir()
	video := &rtsp.TrackInfo{
		Codec:     "H264",
		ClockRate: 90000,
		IsVideo:   true,
		SPS:       []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2},
		PPS:       []byte{0x68, 0xce, 0x38, 0x80},
	}
	rc := NewRecordingConsumer(dir, "test-cam", time.Minute, video, nil, testDisk(t), nil)

	// Three poison packets: each panics inside the real decode path.
	for i := 0; i < 3; i++ {
		rc.OnVideoRTP(nil)
	}
	time.Sleep(200 * time.Millisecond)

	if n := len(rc.pktCh); n != 0 {
		t.Fatalf("processLoop stopped draining after a packet panic: %d packets stuck", n)
	}

	// It must still accept and consume new work after recovering.
	rc.OnVideoRTP(nil)
	time.Sleep(120 * time.Millisecond)
	if n := len(rc.pktCh); n != 0 {
		t.Fatalf("processLoop not consuming after recovery: %d packets stuck", n)
	}

	rc.Close()
}

func TestRecordingConsumer_EnsureSegmentError_PausesAfterRepeatedFailures(t *testing.T) {
	// Use a read-only directory so segment file creation fails
	dir := t.TempDir()
	segDir := filepath.Join(dir, "segments")
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Make the segment directory read-only so os.Create fails
	if err := os.Chmod(segDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(segDir, 0o755) })

	video := &rtsp.TrackInfo{
		Codec:     "H264",
		ClockRate: 90000,
		IsVideo:   true,
		SPS:       []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2},
		PPS:       []byte{0x68, 0xce, 0x38, 0x80},
	}

	rc := NewRecordingConsumer(segDir, "test-cam", time.Minute, video, nil, testDisk(t), nil)
	defer rc.Close()

	// Send enough video packets to trigger 3+ ensureSegment failures
	for i := 0; i < 5; i++ {
		rc.OnVideoRTP(&rtp.Packet{
			Header: rtp.Header{
				PayloadType: 96,
				Timestamp:   uint32(i * 3000),
			},
			Payload: []byte{0x65, 0x00, 0x01}, // fake IDR
		})
	}

	// Wait for processLoop to handle the packets
	time.Sleep(100 * time.Millisecond)

	if !rc.Paused() {
		t.Error("consumer should be paused after repeated segment creation failures")
	}
}
