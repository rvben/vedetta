package media

import (
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/rvben/vedetta/internal/rtsp"
)

// testDiskNoFloor removes the 256 MiB static floor so these tests exercise
// segment handling rather than the free space of whatever volume they run on.
func testDiskNoFloor(t *testing.T) *DiskSpace {
	t.Helper()
	ds := NewDiskSpace(t.TempDir())
	ds.SetThreshold(1, nil)
	return ds
}

// segmentRecorderStub collects the consumer's registration and removal
// callbacks the way the DB does: upsert by path, delete by path.
type segmentRecorderStub struct {
	mu      sync.Mutex
	latest  map[string]SegmentInfo
	removed []string
}

func newSegmentRecorderStub() *segmentRecorderStub {
	return &segmentRecorderStub{latest: map[string]SegmentInfo{}}
}

func (s *segmentRecorderStub) save(info SegmentInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latest[info.Path] = info
}

func (s *segmentRecorderStub) delete(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.latest, path)
	s.removed = append(s.removed, path)
}

func (s *segmentRecorderStub) get(path string) (SegmentInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, ok := s.latest[path]
	return info, ok
}

func (s *segmentRecorderStub) wasRemoved(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.removed {
		if p == path {
			return true
		}
	}
	return false
}

func reliabilityVideoTrack() *rtsp.TrackInfo {
	return &rtsp.TrackInfo{
		Codec: "H264", ClockRate: 90000, IsVideo: true,
		SPS: []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2},
		PPS: []byte{0x68, 0xce, 0x38, 0x80},
	}
}

// waitOpenSegment waits until the consumer has an open segment and returns it.
func waitOpenSegment(t *testing.T, rc *RecordingConsumer) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if p := rc.CurrentSegmentPath(); p != "" {
			return p
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("consumer never opened a segment")
	return ""
}

// waitDrained waits until every queued packet has been processed. Emptying the
// channel is not enough (the last packet is dispatched after the receive), so
// it also takes rc.mu, which the dispatch holds for the whole packet.
func waitDrained(t *testing.T, rc *RecordingConsumer) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(rc.pktCh) == 0 {
			rc.mu.Lock()
			rc.mu.Unlock() //nolint:staticcheck // barrier: waits for the in-flight dispatch
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("consumer never drained its packet queue")
}

// A failed final flush loses every frame since the last keyframe. The consumer
// used to log the close error and then register the file with the writer's full
// media duration, so the DB claimed coverage for video that never reached disk
// and playback ran off the end of the file.
func TestRecordingConsumer_TruncatedSegmentRegistersOnlyDurableMedia(t *testing.T) {
	dir := t.TempDir()
	stub := newSegmentRecorderStub()
	rc := NewRecordingConsumer(dir, "test-cam", time.Minute, reliabilityVideoTrack(), nil, testDiskNoFloor(t), stub.save)
	rc.SetSegmentRemovedHook(stub.delete)

	// Two GOPs: the first is flushed when the second keyframe arrives, the
	// second is only written during Close.
	for i, ts := range []uint32{0, 3000, 6000, 9000, 12000, 15000} {
		nal := byte(0x41)
		if ts == 0 || ts == 9000 {
			nal = 0x65
		}
		rc.OnVideoRTP(h264TestPacket(uint16(i+1), ts, nal))
	}
	path := waitOpenSegment(t, rc)
	waitDrained(t, rc)

	// The volume fails between the last durable write and Close.
	rc.mu.Lock()
	breakWriterFile(t, rc.writer)
	rc.mu.Unlock()

	rc.Close()

	info, ok := stub.get(path)
	if !ok {
		t.Fatalf("segment %s left no record at all", path)
	}
	got := info.EndTime.Sub(info.StartTime)
	want := 100 * time.Millisecond // the one GOP that reached disk
	if got != want {
		t.Fatalf("truncated segment registered as %v of coverage, want %v: the last GOP never reached disk", got, want)
	}
}

// A segment row is registered as soon as the file is created, so both removal
// paths (an empty segment at close, and the post-panic discard) must delete the
// row too. Otherwise the DB points at files that no longer exist: retention
// never reclaims them and every clip extraction on that range fails.
func TestRecordingConsumer_EmptySegmentRemovesItsRow(t *testing.T) {
	dir := t.TempDir()
	stub := newSegmentRecorderStub()
	rc := NewRecordingConsumer(dir, "test-cam", time.Minute, reliabilityVideoTrack(), nil, testDiskNoFloor(t), stub.save)
	rc.SetSegmentRemovedHook(stub.delete)

	// Non-keyframes only: the segment file is created and registered, but the
	// init segment is never written, so the file stays empty.
	rc.OnVideoRTP(h264TestPacket(1, 0, 0x41))
	rc.OnVideoRTP(h264TestPacket(2, 3000, 0x41))
	path := waitOpenSegment(t, rc)

	rc.Close()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("empty segment file %s was not removed: %v", path, err)
	}
	if !stub.wasRemoved(path) {
		t.Fatalf("segment row for %s was left behind after its file was deleted", path)
	}
}

func TestRecordingConsumer_PanicDiscardRemovesItsRow(t *testing.T) {
	dir := t.TempDir()
	stub := newSegmentRecorderStub()
	rc := NewRecordingConsumer(dir, "test-cam", time.Minute, reliabilityVideoTrack(), nil, testDiskNoFloor(t), stub.save)
	rc.SetSegmentRemovedHook(stub.delete)

	rc.OnVideoRTP(h264TestPacket(1, 0, 0x65))
	rc.OnVideoRTP(h264TestPacket(2, 3000, 0x41))
	path := waitOpenSegment(t, rc)
	waitDrained(t, rc)

	// A nil packet panics inside the real decode path; the consumer recovers
	// and discards the poisoned writer along with its file.
	rc.pktCh <- rtpMsg{pkt: nil, video: true}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	rc.Close()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("segment file %s survived the post-panic discard: %v", path, err)
	}
	if !stub.wasRemoved(path) {
		t.Fatalf("segment row for %s was left behind after the post-panic discard deleted its file", path)
	}
}

// The recording queue is deliberately nonblocking, so a slow disk makes it drop
// packets. Dropping silently is the problem: the segment then contains a hole
// with no marker, and the decode errors that follow are swallowed by the
// writer. Drops must be counted, and the segment must be closed at the
// discontinuity so the next one starts from a keyframe.
func TestRecordingConsumer_QueueOverflowCountsDropsAndClosesSegment(t *testing.T) {
	dir := t.TempDir()
	stub := newSegmentRecorderStub()
	rc := NewRecordingConsumer(dir, "test-cam", time.Minute, reliabilityVideoTrack(), nil, testDiskNoFloor(t), stub.save)
	rc.SetSegmentRemovedHook(stub.delete)
	defer rc.Close()

	rc.OnVideoRTP(h264TestPacket(1, 0, 0x65))
	rc.OnVideoRTP(h264TestPacket(2, 3000, 0x41))
	rc.OnVideoRTP(h264TestPacket(3, 6000, 0x41))
	path := waitOpenSegment(t, rc)
	waitDrained(t, rc)

	// Stall the processing loop and overrun the queue, the way a slow volume
	// does in production.
	const flood = 700
	rc.mu.Lock()
	for i := 0; i < flood; i++ {
		rc.OnVideoRTP(h264TestPacket(uint16(4+i), uint32(9000+i*3000), 0x41))
	}
	rc.mu.Unlock()
	waitDrained(t, rc)

	if n := rc.DroppedPackets(); n == 0 {
		t.Fatalf("DroppedPackets() = 0 after overrunning a %d-deep queue with %d packets: drops are invisible", cap(rc.pktCh), flood)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if info, ok := stub.get(path); ok && info.SizeBytes > 0 {
			return // segment was finalized at the discontinuity
		}
		time.Sleep(5 * time.Millisecond)
	}
	info, _ := stub.get(path)
	t.Fatalf("segment %s was not closed after packets were dropped (size %d): the hole is written into the middle of a segment that still claims to be continuous", path, info.SizeBytes)
}

// On a wedged volume os.Create never returns, so each retry strands the create
// goroutine plus the goroutine that reaps its result. Recording retries every
// 30s for as long as the volume is out, so the leak is unbounded. Only one
// create may be outstanding per consumer.
func TestRecordingConsumer_StalledCreate_SingleOutstandingAttempt(t *testing.T) {
	dir := t.TempDir()
	rc := NewRecordingConsumer(dir, "test-cam", time.Minute, reliabilityVideoTrack(), nil, testDiskNoFloor(t), nil)
	defer rc.Close()

	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	var calls atomic.Int64
	// Set before any packet is sent: processLoop only reads these after
	// receiving a packet, so the channel send establishes happens-before.
	rc.createTimeout = 100 * time.Millisecond
	rc.newWriter = func(string, *rtsp.TrackInfo, *rtsp.TrackInfo) (*SegmentWriter, error) {
		calls.Add(1)
		<-block // a stalled volume: creation never returns
		return nil, nil
	}

	for i := 0; i < 6; i++ {
		rc.OnVideoRTP(&rtp.Packet{
			Header:  rtp.Header{PayloadType: 96, Timestamp: uint32(i * 3000)},
			Payload: []byte{0x65, 0x00, 0x01},
		})
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !rc.Paused() {
		time.Sleep(10 * time.Millisecond)
	}
	if !rc.Paused() {
		t.Fatal("consumer never paused on a stalled volume")
	}

	if n := calls.Load(); n != 1 {
		t.Fatalf("segment creation was attempted %d times with one create already outstanding, want 1: each extra attempt strands two goroutines that never return", n)
	}
	if !rc.SegmentCreatePending() {
		t.Fatal("SegmentCreatePending() = false while a create is stalled in the kernel")
	}
}

// A volume slow enough to overrun the queue keeps overrunning it, so the
// discontinuity marker is raised again between every processed packet. Closing
// the segment on each one turns a slow disk into a create/delete storm: each
// packet finalizes a file, deletes it because it holds nothing yet, retracts
// its DB row, and opens the next. Rotation at a gap has to be bounded.
func TestRecordingConsumer_SustainedDrops_DoNotThrashSegments(t *testing.T) {
	dir := t.TempDir()
	stub := newSegmentRecorderStub()
	rc := NewRecordingConsumer(dir, "test-cam", time.Minute, reliabilityVideoTrack(), nil, testDiskNoFloor(t), stub.save)
	rc.SetSegmentRemovedHook(stub.delete)
	defer rc.Close()

	// Set before the first packet: processLoop only reads newWriter after a
	// receive, so the channel send establishes happens-before.
	var writers atomic.Int64
	realWriter := rc.newWriter
	rc.newWriter = func(path string, video, audio *rtsp.TrackInfo) (*SegmentWriter, error) {
		writers.Add(1)
		return realWriter(path, video, audio)
	}

	// Packets are dispatched inline rather than queued: a drop has to land
	// between two processed packets for this to be a sustained overrun, and
	// the queue would reorder that.
	rc.dispatch(rtpMsg{pkt: h264TestPacket(1, 0, 0x65), video: true})
	if rc.CurrentSegmentPath() == "" {
		t.Fatal("consumer never opened a segment")
	}

	const packets = 200
	for i := 0; i < packets; i++ {
		// Keyframes keep arriving through the overrun, so each fresh segment
		// takes media and becomes eligible to rotate all over again.
		nal := byte(0x41)
		if i%10 == 0 {
			nal = 0x65
		}
		rc.noteDrop()
		rc.dispatch(rtpMsg{pkt: h264TestPacket(uint16(2+i), uint32(3000*(i+1)), nal), video: true})
	}

	if n := rc.DroppedPackets(); n < packets {
		t.Fatalf("DroppedPackets() = %d, want >= %d: the drops under test were not recorded", n, packets)
	}
	if n := writers.Load(); n > 3 {
		t.Fatalf("%d segment writers created for %d packets after a keyframe: the discontinuity rotation is per-packet, so a slow volume churns files and DB rows", n, packets)
	}
	if n := len(stub.removed); n > 2 {
		t.Fatalf("%d segments were created and deleted again: %v", n, stub.removed)
	}
}

// A segment that has not taken its opening keyframe yet holds no media, so
// closing it at a gap deletes an empty file and retracts the DB row written
// when it was created, only to open an identical one. The gap is already where
// the next segment would start; there is nothing to split.
func TestRecordingConsumer_DropBeforeFirstKeyframe_KeepsSegment(t *testing.T) {
	dir := t.TempDir()
	stub := newSegmentRecorderStub()
	rc := NewRecordingConsumer(dir, "test-cam", time.Minute, reliabilityVideoTrack(), nil, testDiskNoFloor(t), stub.save)
	rc.SetSegmentRemovedHook(stub.delete)
	defer rc.Close()

	// A P-frame opens the file but is not written: the writer waits for a
	// keyframe, so the segment carries no media.
	rc.dispatch(rtpMsg{pkt: h264TestPacket(1, 0, 0x41), video: true})
	path := rc.CurrentSegmentPath()
	if path == "" {
		t.Fatal("consumer never opened a segment")
	}

	rc.noteDrop()
	rc.dispatch(rtpMsg{pkt: h264TestPacket(2, 3000, 0x41), video: true})

	if got := rc.CurrentSegmentPath(); got != path {
		t.Fatalf("segment rotated from %s to %s at a gap that arrived before the first keyframe", path, got)
	}
	if n := len(stub.removed); n != 0 {
		t.Fatalf("empty segment was created and deleted again at the gap: %v", stub.removed)
	}
}
