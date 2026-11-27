package media

import (
	"testing"
	"time"

	"github.com/pion/rtp"
)

const (
	idrSlice   = 0x65
	deltaSlice = 0x41
)

// markedFrame is a one-packet frame whose payload can be found in a segment
// file by its mark.
func markedFrame(seq uint16, ts uint32, nal, mark byte) *rtp.Packet {
	pkt := markedPacket(seq, ts, mark)
	pkt.Payload[0] = nal
	return pkt
}

// sendFrame hands the consumer a frame and waits until it has been processed.
func sendFrame(t *testing.T, rc *RecordingConsumer, pkt *rtp.Packet) {
	t.Helper()
	rc.OnVideoRTP(pkt)
	waitDrained(t, rc)
}

// A new file can only start at a keyframe. A segment that has run its length
// therefore keeps the frames up to the stream's next keyframe, and that
// keyframe opens the next segment: every frame is in exactly one file, and the
// second file starts where the first one stops.
func TestRecordingConsumer_RotationWaitsForAKeyframe(t *testing.T) {
	withoutBackstop(t)
	const segLen = 50 * time.Millisecond
	const frame = 4500 // 90 kHz ticks, 1/20 s: not the writer's fallback duration
	stub := newSegmentRecorderStub()
	rc := NewRecordingConsumer(t.TempDir(), "test-cam", segLen, reliabilityVideoTrack(), nil, testDiskNoFloor(t), stub.save)

	sendFrame(t, rc, markedFrame(1, 0*frame, idrSlice, 0xA1))
	sendFrame(t, rc, markedFrame(2, 1*frame, deltaSlice, 0xA2))
	first := waitOpenSegment(t, rc)
	time.Sleep(2 * segLen)
	// The segment is over its length now, in the middle of a GOP.
	sendFrame(t, rc, markedFrame(3, 2*frame, deltaSlice, 0xA3))
	sendFrame(t, rc, markedFrame(4, 3*frame, deltaSlice, 0xA4))
	if got := rc.CurrentSegmentPath(); got != first {
		t.Fatal("the segment was rotated before a keyframe arrived")
	}
	sendFrame(t, rc, markedFrame(5, 4*frame, idrSlice, 0xB1))
	keyframeArrived := time.Now()
	time.Sleep(40 * time.Millisecond)
	// A frame is written when the next one starts, so this is the packet that
	// completes the keyframe and moves it to a new segment.
	sendFrame(t, rc, markedFrame(6, 5*frame, deltaSlice, 0xB2))
	second := waitOpenSegment(t, rc)
	// While the segment is open its row already starts at the keyframe.
	if open, ok := stub.get(second); !ok || open.StartTime.After(keyframeArrived) {
		t.Errorf("the open second segment is registered from %v, after its keyframe arrived at %v (registered: %v)", open.StartTime, keyframeArrived, ok)
	}
	// It can run for its length and then wait for a keyframe, so that is how
	// far its row reaches until it closes.
	if open, _ := stub.get(second); open.EndTime.Sub(open.StartTime) != segLen+rotationKeyframeGrace {
		t.Errorf("the open second segment is registered for %v, want its length plus the keyframe grace (%v)", open.EndTime.Sub(open.StartTime), segLen+rotationKeyframeGrace)
	}
	sendFrame(t, rc, markedFrame(7, 6*frame, deltaSlice, 0xB3))
	rc.Close()

	if second == first {
		t.Fatal("the segment was not rotated at the keyframe after it ran its length")
	}
	for _, mark := range []byte{0xA1, 0xA2, 0xA3, 0xA4} {
		if !segmentHolds(t, first, mark) {
			t.Errorf("the first segment lacks frame %#x", mark)
		}
		if segmentHolds(t, second, mark) {
			t.Errorf("frame %#x of the first GOP is in the second segment", mark)
		}
	}
	for _, mark := range []byte{0xB1, 0xB2, 0xB3} {
		if !segmentHolds(t, second, mark) {
			t.Errorf("the second segment lacks frame %#x", mark)
		}
		if segmentHolds(t, first, mark) {
			t.Errorf("frame %#x of the second GOP is in the first segment", mark)
		}
	}

	a, okA := stub.get(first)
	b, okB := stub.get(second)
	if !okA || !okB {
		t.Fatalf("segments registered: first %v, second %v", okA, okB)
	}
	if got, want := a.EndTime.Sub(a.StartTime), 4*time.Second/20; got != want {
		t.Errorf("the first segment covers %v, want its four frames (%v)", got, want)
	}
	if got, want := b.EndTime.Sub(b.StartTime), 3*time.Second/20; got != want {
		t.Errorf("the second segment covers %v, want its three frames (%v)", got, want)
	}
	if b.StartTime.After(keyframeArrived) {
		t.Errorf("the second segment starts %v after its keyframe arrived", b.StartTime.Sub(keyframeArrived))
	}
}

// A stream that sends no keyframe must not grow one segment without bound: it
// is cut once the grace period has passed, and not before.
func TestRecordingConsumer_RotationStopsWaitingAfterTheGrace(t *testing.T) {
	withoutBackstop(t)
	const segLen = 50 * time.Millisecond
	const grace = 150 * time.Millisecond
	stub := newSegmentRecorderStub()
	rc := NewRecordingConsumer(t.TempDir(), "test-cam", segLen, reliabilityVideoTrack(), nil, testDiskNoFloor(t), stub.save)
	rc.keyframeGrace = grace

	sendFrame(t, rc, markedFrame(1, 0, idrSlice, 0xA1))
	sendFrame(t, rc, markedFrame(2, 3000, deltaSlice, 0xA2))
	first := waitOpenSegment(t, rc)
	time.Sleep(segLen + grace/3)
	sendFrame(t, rc, markedFrame(3, 6000, deltaSlice, 0xA3))
	sendFrame(t, rc, markedFrame(4, 9000, deltaSlice, 0xA4))
	if got := rc.CurrentSegmentPath(); got != first {
		t.Fatal("the segment was cut within the grace period")
	}
	time.Sleep(grace)
	sendFrame(t, rc, markedFrame(5, 12000, deltaSlice, 0xA5))
	second := waitOpenSegment(t, rc)
	rc.Close()

	if second == first {
		t.Fatal("the segment was never cut although no keyframe arrived")
	}
	for _, mark := range []byte{0xA1, 0xA2, 0xA3, 0xA4} {
		if !segmentHolds(t, first, mark) {
			t.Errorf("the first segment lacks frame %#x", mark)
		}
	}
}

// A segment is ended on a video frame boundary. When the video stops after
// the segment has run its length and only audio keeps arriving, there is no
// such boundary, and the segment is closed all the same.
func TestRecordingConsumer_RotationDoesNotWaitForVideoThatStopped(t *testing.T) {
	withBackstop(t, 5*time.Millisecond)
	const segLen = 50 * time.Millisecond
	const grace = 400 * time.Millisecond
	stub := newSegmentRecorderStub()
	rc := NewRecordingConsumer(t.TempDir(), "test-cam", segLen, reliabilityVideoTrack(), nil, testDiskNoFloor(t), stub.save)
	rc.keyframeGrace = grace

	sendFrame(t, rc, markedFrame(1, 0, idrSlice, 0xA1))
	sendFrame(t, rc, markedFrame(2, 3000, deltaSlice, 0xA2))
	first := waitOpenSegment(t, rc)
	time.Sleep(segLen + grace/5)
	sendFrame(t, rc, markedFrame(3, 6000, deltaSlice, 0xA3))
	if got := rc.CurrentSegmentPath(); got != first {
		t.Fatal("the segment was cut within the grace period")
	}
	seq := uint16(1)
	deadline := time.Now().Add(segLen + 10*grace)
	for rc.CurrentSegmentPath() == first && time.Now().Before(deadline) {
		rc.OnAudioRTP(audioTestPacket(seq, uint32(seq)*160))
		seq++
		time.Sleep(10 * time.Millisecond)
	}
	open := rc.CurrentSegmentPath()
	rc.Close()

	if open == first {
		t.Fatal("the segment stayed open although its video stopped after its deadline")
	}
	for _, mark := range []byte{0xA1, 0xA2, 0xA3} {
		if !segmentHolds(t, first, mark) {
			t.Errorf("the segment lacks frame %#x", mark)
		}
	}
}

// A stream whose video can no longer be assembled into frames never reaches
// the frame boundary a split waits for. Its segment is closed all the same.
func TestRecordingConsumer_RotationDoesNotWaitForFramesThatNeverComplete(t *testing.T) {
	withoutBackstop(t)
	const segLen = 50 * time.Millisecond
	const grace = 50 * time.Millisecond
	stub := newSegmentRecorderStub()
	rc := NewRecordingConsumer(t.TempDir(), "test-cam", segLen, reliabilityVideoTrack(), nil, testDiskNoFloor(t), stub.save)
	rc.keyframeGrace = grace

	sendFrame(t, rc, markedFrame(1, 0, idrSlice, 0xA1))
	sendFrame(t, rc, markedFrame(2, 3000, deltaSlice, 0xA2))
	first := waitOpenSegment(t, rc)
	// Every frame from here on lacks its last fragment.
	seq, ts := uint16(3), uint32(6000)
	deadline := time.Now().Add(segLen + 10*grace)
	for rc.CurrentSegmentPath() == first && time.Now().Before(deadline) {
		sendFrame(t, rc, fragmentedFrame(seq, ts, deltaSlice, 0xEE)[0])
		seq, ts = seq+2, ts+3000
		time.Sleep(5 * time.Millisecond)
	}
	second := rc.CurrentSegmentPath()
	rc.Close()

	if second == first {
		t.Fatal("the segment stayed open although no frame completed after its deadline")
	}
	b, ok := stub.get(first)
	if !ok {
		t.Fatal("the first segment was not registered")
	}
	if !segmentHolds(t, first, 0xA1) {
		t.Error("the first segment lacks its keyframe")
	}
	if got, want := b.EndTime.Sub(b.StartTime), 6000*time.Second/90000; got != want {
		t.Errorf("the first segment covers %v, want the two frames it holds (%v)", got, want)
	}
}

// fragmentedFrame is a frame sent as two FU-A fragments; reassembled, its
// payload can be found in a segment file by its mark.
func fragmentedFrame(seq uint16, ts uint32, nal, mark byte) []*rtp.Packet {
	const fuIndicator = 0x7C // NRI 3, type 28 (FU-A)
	nalType := nal & 0x1F
	start := h264TestPacket(seq, ts, nal)
	start.Payload = []byte{fuIndicator, 0x80 | nalType, 0x88, 0x84, mark, mark, mark}
	end := h264TestPacket(seq+1, ts, nal)
	end.Payload = []byte{fuIndicator, 0x40 | nalType, mark, mark, mark}
	return []*rtp.Packet{start, end}
}

// The keyframe that opens a new segment is only complete once the frame after
// it has delivered a whole NAL unit, which for a fragmented frame is several
// packets in. Those packets belong to the new segment as well.
func TestRecordingConsumer_RotationKeepsTheFragmentedFrameAfterTheKeyframe(t *testing.T) {
	withoutBackstop(t)
	const segLen = 50 * time.Millisecond
	const frame = 4500
	stub := newSegmentRecorderStub()
	rc := NewRecordingConsumer(t.TempDir(), "test-cam", segLen, reliabilityVideoTrack(), nil, testDiskNoFloor(t), stub.save)

	sendFrame(t, rc, markedFrame(1, 0*frame, idrSlice, 0xA1))
	sendFrame(t, rc, markedFrame(2, 1*frame, deltaSlice, 0xA2))
	first := waitOpenSegment(t, rc)
	time.Sleep(2 * segLen)
	sendFrame(t, rc, markedFrame(3, 2*frame, idrSlice, 0xB1))
	keyframeArrived := time.Now()
	for _, pkt := range fragmentedFrame(4, 3*frame, deltaSlice, 0xB2) {
		time.Sleep(40 * time.Millisecond)
		rc.OnVideoRTP(pkt)
	}
	sendFrame(t, rc, markedFrame(6, 4*frame, deltaSlice, 0xB3))
	second := waitOpenSegment(t, rc)
	sendFrame(t, rc, markedFrame(7, 5*frame, deltaSlice, 0xB4))
	rc.Close()

	if second == first {
		t.Fatal("the segment was not rotated at the keyframe after it ran its length")
	}
	for _, mark := range []byte{0xB1, 0xB2, 0xB3, 0xB4} {
		if !segmentHolds(t, second, mark) {
			t.Errorf("the second segment lacks frame %#x", mark)
		}
		if segmentHolds(t, first, mark) {
			t.Errorf("frame %#x of the second GOP is in the first segment", mark)
		}
	}
	b, ok := stub.get(second)
	if !ok {
		t.Fatal("the second segment was not registered")
	}
	if got, want := b.EndTime.Sub(b.StartTime), 4*time.Second/20; got != want {
		t.Errorf("the second segment covers %v, want its four frames (%v)", got, want)
	}
	// The keyframe's arrival, not that of the fragments that completed it.
	if b.StartTime.After(keyframeArrived) {
		t.Errorf("the second segment starts %v after its keyframe arrived", b.StartTime.Sub(keyframeArrived))
	}
}

// A keyframe that arrives within the grace period opens the next segment even
// when the packet that completes it arrives after the period has passed.
func TestRecordingConsumer_RotationCarriesAKeyframeAcrossTheGraceDeadline(t *testing.T) {
	withoutBackstop(t)
	// The second segment is as old as its keyframe, which arrived a grace
	// period before the segment opens. It has to be long enough not to be due
	// for a split of its own by then.
	const segLen = 400 * time.Millisecond
	const grace = 300 * time.Millisecond
	stub := newSegmentRecorderStub()
	rc := NewRecordingConsumer(t.TempDir(), "test-cam", segLen, reliabilityVideoTrack(), nil, testDiskNoFloor(t), stub.save)
	rc.keyframeGrace = grace

	sendFrame(t, rc, markedFrame(1, 0, idrSlice, 0xA1))
	sendFrame(t, rc, markedFrame(2, 3000, deltaSlice, 0xA2))
	first := waitOpenSegment(t, rc)
	time.Sleep(segLen + grace/3)
	sendFrame(t, rc, markedFrame(3, 6000, deltaSlice, 0xA3))
	sendFrame(t, rc, markedFrame(4, 9000, idrSlice, 0xB1))
	if got := rc.CurrentSegmentPath(); got != first {
		t.Fatal("the segment was cut within the grace period")
	}
	time.Sleep(grace)
	sendFrame(t, rc, markedFrame(5, 12000, deltaSlice, 0xB2))
	second := waitOpenSegment(t, rc)
	sendFrame(t, rc, markedFrame(6, 15000, deltaSlice, 0xB3))
	rc.Close()

	if second == first {
		t.Fatal("the segment was never cut")
	}
	for _, mark := range []byte{0xA1, 0xA2, 0xA3} {
		if !segmentHolds(t, first, mark) {
			t.Errorf("the first segment lacks frame %#x", mark)
		}
	}
	for _, mark := range []byte{0xB1, 0xB2, 0xB3} {
		if !segmentHolds(t, second, mark) {
			t.Errorf("the second segment lacks frame %#x", mark)
		}
		if segmentHolds(t, first, mark) {
			t.Errorf("frame %#x of the second GOP is in the first segment", mark)
		}
	}
}

// A stream that resumes after a stall starts a new segment, and it resumes on
// a keyframe. That keyframe arrives in fragments, all of which belong to the
// new segment.
func TestRecordingConsumer_TimestampGapKeepsTheFragmentedKeyframe(t *testing.T) {
	withoutBackstop(t)
	stub := newSegmentRecorderStub()
	rc := NewRecordingConsumer(t.TempDir(), "test-cam", time.Minute, reliabilityVideoTrack(), nil, testDiskNoFloor(t), stub.save)

	sendFrame(t, rc, markedFrame(1, 0, idrSlice, 0xA1))
	sendFrame(t, rc, markedFrame(2, 3000, deltaSlice, 0xA2))
	first := waitOpenSegment(t, rc)
	const resume = 3000 + 60*90000
	for _, pkt := range fragmentedFrame(3, resume, idrSlice, 0xB1) {
		rc.OnVideoRTP(pkt)
	}
	sendFrame(t, rc, markedFrame(5, resume+3000, deltaSlice, 0xB2))
	second := waitOpenSegment(t, rc)
	sendFrame(t, rc, markedFrame(6, resume+6000, deltaSlice, 0xB3))
	rc.Close()

	if second == first {
		t.Fatal("the stall did not start a new segment")
	}
	for _, mark := range []byte{0xB1, 0xB2, 0xB3} {
		if !segmentHolds(t, second, mark) {
			t.Errorf("the segment after the stall lacks frame %#x", mark)
		}
		if segmentHolds(t, first, mark) {
			t.Errorf("frame %#x from after the stall is in the segment before it", mark)
		}
	}
	b, ok := stub.get(second)
	if !ok {
		t.Fatal("the segment after the stall was not registered")
	}
	if got, want := b.EndTime.Sub(b.StartTime), 3*time.Second/30; got != want {
		t.Errorf("the segment after the stall covers %v, want its three frames (%v)", got, want)
	}
}
