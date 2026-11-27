package media

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/pion/rtp"

	"github.com/rvben/vedetta/internal/rtsp"
)

// idleConsumer returns a consumer whose processLoop is not running, so a test
// can count the wake-ups a packet would cause.
func idleConsumer(video *rtsp.TrackInfo) *RecordingConsumer {
	return &RecordingConsumer{
		videoTrack: video,
		pktCh:      make(chan rtpMsg, recordingQueueDepth),
		wake:       make(chan struct{}, 1),
	}
}

// woken reports whether a wake-up is pending, and clears it.
func woken(rc *RecordingConsumer) bool {
	select {
	case <-rc.wake:
		return true
	default:
		return false
	}
}

// withBackstop sets the backstop interval of consumers built during the test.
func withBackstop(t *testing.T, d time.Duration) {
	t.Helper()
	old := recordingBackstopInterval
	recordingBackstopInterval = d
	t.Cleanup(func() { recordingBackstopInterval = old })
}

// waitIdle waits until the loop has stopped its backstop and gone back to
// waiting.
func waitIdle(t *testing.T, rc *RecordingConsumer) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !rc.idle.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the loop never went idle")
		}
		time.Sleep(time.Millisecond)
	}
	rc.drainMu.Lock()
	rc.drainMu.Unlock() //nolint:staticcheck // barrier: the loop has looked at the queue and is waiting again
}

// withoutBackstop makes consumers built during the test wait for an explicit
// wake-up only, so a packet that is processed proves something woke the loop.
func withoutBackstop(t *testing.T) {
	withBackstop(t, time.Hour)
}

func audioTestPacket(seq uint16, ts uint32) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 8, SequenceNumber: seq, Timestamp: ts, SSRC: 0xBEEF},
		Payload: make([]byte, 160),
	}
}

func TestRecordingConsumer_WakesOncePerVideoFrame(t *testing.T) {
	rc := idleConsumer(reliabilityVideoTrack())

	// The stream's first packet may carry any timestamp, zero included.
	rc.OnVideoRTP(h264TestPacket(1, 0, 0x65))
	if !woken(rc) {
		t.Fatal("the first packet of the stream did not wake the loop")
	}
	for seq := uint16(2); seq <= 40; seq++ {
		rc.OnVideoRTP(h264TestPacket(seq, 0, 0x65))
		if woken(rc) {
			t.Fatalf("packet %d of the same frame woke the loop", seq)
		}
	}
	rc.OnVideoRTP(h264TestPacket(41, 3000, 0x41))
	if !woken(rc) {
		t.Fatal("the first packet of the next frame did not wake the loop")
	}
	rc.OnVideoRTP(h264TestPacket(42, 3000, 0x41))
	if woken(rc) {
		t.Fatal("the second packet of the next frame woke the loop")
	}
	if n := len(rc.pktCh); n != 42 {
		t.Fatalf("queued %d packets, want 42", n)
	}
}

// A frame larger than half the queue cannot wait for the next frame: the
// packets behind it would be dropped.
func TestRecordingConsumer_WakesWhenTheQueueIsHalfFull(t *testing.T) {
	rc := idleConsumer(reliabilityVideoTrack())
	rc.OnVideoRTP(h264TestPacket(0, 9000, 0x65))
	woken(rc)

	for i := 1; i < recordingQueueDepth/2-1; i++ {
		rc.OnVideoRTP(h264TestPacket(uint16(i), 9000, 0x65))
		if woken(rc) {
			t.Fatalf("woke with %d of %d packets queued", len(rc.pktCh), recordingQueueDepth)
		}
	}
	rc.OnVideoRTP(h264TestPacket(1000, 9000, 0x65))
	if len(rc.pktCh) != recordingQueueDepth/2 || !woken(rc) {
		t.Fatalf("no wake-up with %d of %d packets queued", len(rc.pktCh), recordingQueueDepth)
	}
	rc.OnAudioRTP(audioTestPacket(1, 160))
	if !woken(rc) {
		t.Fatal("an audio packet past the half-way mark did not wake the loop")
	}
}

func TestRecordingConsumer_AudioWaitsForVideoUnlessThereIsNone(t *testing.T) {
	withVideo := idleConsumer(reliabilityVideoTrack())
	withVideo.OnAudioRTP(audioTestPacket(1, 160))
	if woken(withVideo) {
		t.Fatal("an audio packet woke the loop of a stream that has video")
	}
	if len(withVideo.pktCh) != 1 {
		t.Fatal("the audio packet was not queued")
	}

	audioOnly := idleConsumer(nil)
	for seq := uint16(1); seq <= 3; seq++ {
		audioOnly.OnAudioRTP(audioTestPacket(seq, uint32(seq)*160))
		if !woken(audioOnly) {
			t.Fatalf("audio packet %d of an audio-only stream did not wake the loop", seq)
		}
	}
}

// With the backstop out of the picture, only the wake-up from the next
// frame's first packet can empty the queue.
func TestRecordingConsumer_NextFrameFlushesTheQueuedOne(t *testing.T) {
	withoutBackstop(t)
	stub := newSegmentRecorderStub()
	rc := NewRecordingConsumer(t.TempDir(), "test-cam", time.Minute, reliabilityVideoTrack(), nil, testDiskNoFloor(t), stub.save)
	defer rc.Close()

	seq := uint16(0)
	for frame := range uint32(5) {
		nal := byte(0x41)
		if frame == 0 {
			nal = 0x65
		}
		for range 3 {
			seq++
			rc.OnVideoRTP(h264TestPacket(seq, frame*3000, nal))
		}
		rc.OnAudioRTP(audioTestPacket(seq, frame*160))
	}
	rc.OnVideoRTP(h264TestPacket(seq+1, 5*3000, 0x41))
	waitDrained(t, rc)
	waitOpenSegment(t, rc)
}

func TestRecordingConsumer_AudioOnlyStreamIsDrained(t *testing.T) {
	withoutBackstop(t)
	rc := NewRecordingConsumer(t.TempDir(), "test-cam", time.Minute, nil, nil, testDiskNoFloor(t), nil)
	defer rc.Close()

	for seq := uint16(1); seq <= 20; seq++ {
		rc.OnAudioRTP(audioTestPacket(seq, uint32(seq)*160))
	}
	waitDrained(t, rc)
}

// Audio queued behind a video stream that has stopped sending frames, and the
// tail of a stream's last frame, have no next frame to wake the loop. The
// wake-up for the frame's first packet arms the backstop that takes them.
func TestRecordingConsumer_BackstopDrainsWhatNothingElseWakes(t *testing.T) {
	withBackstop(t, 300*time.Millisecond)
	rc := NewRecordingConsumer(t.TempDir(), "test-cam", time.Minute, reliabilityVideoTrack(), nil, testDiskNoFloor(t), nil)
	defer rc.Close()

	// Let the backstop armed at start run out, so only a wake-up can arm it.
	waitIdle(t, rc)
	rc.OnVideoRTP(h264TestPacket(1, 0, 0x65))
	waitDrained(t, rc)
	if rc.idle.Load() {
		t.Fatal("the loop is still marked idle after a wake-up: every packet would wake it")
	}

	rc.OnVideoRTP(h264TestPacket(2, 0, 0x65))
	rc.OnAudioRTP(audioTestPacket(1, 160))
	if n := len(rc.pktCh); n != 2 {
		t.Fatalf("%d packets queued, want 2: something other than the backstop took them", n)
	}
	waitDrained(t, rc)
}

// Close must process the packets no wake-up announced: the rest of the last
// frame, which holds the end of the recording.
func TestRecordingConsumer_CloseDrainsUnannouncedPackets(t *testing.T) {
	withoutBackstop(t)
	rc := NewRecordingConsumer(t.TempDir(), "test-cam", time.Minute, reliabilityVideoTrack(), nil, testDiskNoFloor(t), nil)

	rc.OnVideoRTP(h264TestPacket(1, 0, 0x65))
	waitDrained(t, rc)
	for seq := uint16(2); seq <= 10; seq++ {
		rc.OnVideoRTP(h264TestPacket(seq, 0, 0x65))
	}
	rc.OnAudioRTP(audioTestPacket(1, 160))
	if n := len(rc.pktCh); n != 10 {
		t.Fatalf("%d packets queued before Close, want 10: something drained them early", n)
	}
	rc.Close()
	if n := len(rc.pktCh); n != 0 {
		t.Fatalf("Close left %d packets unprocessed", n)
	}
}

// A disconnect arrives with the rest of the last frame still queued. Those
// packets belong to the segment being closed, not to a new one.
func TestRecordingConsumer_DisconnectWritesTheQueuedTailFirst(t *testing.T) {
	withoutBackstop(t)
	stub := newSegmentRecorderStub()
	rc := NewRecordingConsumer(t.TempDir(), "test-cam", time.Minute, reliabilityVideoTrack(), nil, testDiskNoFloor(t), stub.save)
	defer rc.Close()

	// One wake-up at a time, so none is left pending to take the tail.
	rc.OnVideoRTP(h264TestPacket(1, 0, 0x65))
	waitDrained(t, rc)
	rc.OnVideoRTP(h264TestPacket(2, 3000, 0x41))
	waitDrained(t, rc)
	path := waitOpenSegment(t, rc)
	for seq := uint16(3); seq <= 8; seq++ {
		rc.OnVideoRTP(h264TestPacket(seq, 3000, 0x41))
	}
	rc.OnAudioRTP(audioTestPacket(1, 160))
	if n := len(rc.pktCh); n != 7 {
		t.Fatalf("%d packets queued before the disconnect, want 7", n)
	}

	rc.OnDisconnect()

	if n := len(rc.pktCh); n != 0 {
		t.Fatalf("the disconnect closed the segment with %d packets still queued", n)
	}
	if info, ok := stub.get(path); !ok || info.SizeBytes == 0 {
		t.Fatalf("segment %s was not finalized by the disconnect", path)
	}
	if p := rc.CurrentSegmentPath(); p != "" {
		t.Fatalf("the queued tail opened a new segment %s after the disconnect", p)
	}
}

// A camera that sends nothing must not keep its recording loop polling: the
// backstop stops once the queue has stayed empty, and the next packet of any
// kind starts it again.
func TestRecordingConsumer_IdleLoopStopsPollingUntilAPacketArrives(t *testing.T) {
	withBackstop(t, 5*time.Millisecond)
	rc := NewRecordingConsumer(t.TempDir(), "test-cam", time.Minute, reliabilityVideoTrack(), nil, testDiskNoFloor(t), nil)
	defer rc.Close()

	rc.OnVideoRTP(h264TestPacket(1, 0, 0x65))
	waitIdle(t, rc)

	// Queued behind the loop's back, so nothing announces it: a loop that
	// still polled would take it within one interval.
	rc.pktCh <- rtpMsg{pkt: audioTestPacket(1, 160)}
	time.Sleep(100 * time.Millisecond)
	if n := len(rc.pktCh); n != 1 {
		t.Fatalf("an idle loop took an unannounced packet (%d queued): it is still polling", n)
	}

	// Audio does not wake a busy loop of a stream with video, but it must
	// wake an idle one, or it would wait for video that may never come.
	rc.OnAudioRTP(audioTestPacket(2, 320))
	waitDrained(t, rc)
	if n := len(rc.pktCh); n != 0 {
		t.Fatalf("%d packets left after the wake-up", n)
	}
}

func TestRecordingConsumer_PacketWakesAnIdleLoop(t *testing.T) {
	rc := idleConsumer(reliabilityVideoTrack())
	rc.OnVideoRTP(h264TestPacket(1, 3000, 0x41))
	woken(rc)

	rc.idle.Store(true)
	rc.OnVideoRTP(h264TestPacket(2, 3000, 0x41))
	if !woken(rc) {
		t.Fatal("a packet inside a frame did not wake an idle loop")
	}
	rc.OnAudioRTP(audioTestPacket(1, 160))
	if !woken(rc) {
		t.Fatal("an audio packet did not wake an idle loop")
	}
}

// markedPacket is a one-NAL packet whose payload can be found in a segment
// file afterwards.
func markedPacket(seq uint16, ts uint32, mark byte) *rtp.Packet {
	pkt := h264TestPacket(seq, ts, 0x65)
	pkt.Payload = []byte{0x65, 0x88, 0x84, mark, mark, mark, mark, mark, mark}
	return pkt
}

func segmentHolds(t *testing.T, path string, mark byte) bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Contains(data, bytes.Repeat([]byte{mark}, 6))
}

// The rest of a frame is processed when the next frame starts, which can be
// after the segment has run its length. The whole frame still belongs to the
// segment its first packet went to.
func TestRecordingConsumer_RotationKeepsAFrameInOneSegment(t *testing.T) {
	withoutBackstop(t)
	const segLen = 50 * time.Millisecond
	stub := newSegmentRecorderStub()
	rc := NewRecordingConsumer(t.TempDir(), "test-cam", segLen, reliabilityVideoTrack(), nil, testDiskNoFloor(t), stub.save)

	rc.OnVideoRTP(markedPacket(1, 0, 0xA1))
	waitDrained(t, rc)
	first := waitOpenSegment(t, rc)
	rc.OnVideoRTP(markedPacket(2, 0, 0xA2))
	rc.OnVideoRTP(markedPacket(3, 0, 0xA3))
	time.Sleep(2 * segLen)
	rc.OnVideoRTP(markedPacket(4, 3000, 0xB1))
	waitDrained(t, rc)
	second := waitOpenSegment(t, rc)
	rc.Close()

	if second == first {
		t.Fatal("the segment was not rotated after it ran its length")
	}
	for _, mark := range []byte{0xA1, 0xA2, 0xA3} {
		if !segmentHolds(t, first, mark) {
			t.Errorf("the first segment lacks slice %#x of its last frame", mark)
		}
		if segmentHolds(t, second, mark) {
			t.Errorf("slice %#x of the first frame went to the second segment", mark)
		}
	}
	if segmentHolds(t, first, 0xB1) || !segmentHolds(t, second, 0xB1) {
		t.Error("the frame that started after the rotation is not in the second segment alone")
	}
}
