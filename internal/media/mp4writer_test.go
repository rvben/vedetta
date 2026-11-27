package media

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/pion/rtp"

	"github.com/rvben/vedetta/internal/rtsp"
)

func TestSegmentWriter_WriteAndClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.mp4")

	sps := []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2}
	pps := []byte{0x68, 0xce, 0x38, 0x80}

	video := &rtsp.TrackInfo{
		Codec:     "H264",
		ClockRate: 90000,
		IsVideo:   true,
		SPS:       sps,
		PPS:       pps,
	}

	sw, err := NewSegmentWriter(path, video, nil)
	if err != nil {
		t.Fatalf("NewSegmentWriter: %v", err)
	}

	dur, err := sw.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	if dur <= 0 {
		t.Errorf("duration = %v, want > 0", dur)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info == nil {
		t.Fatal("file doesn't exist")
	}
}

func TestSegmentWriter_VideoOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "video_only.mp4")

	video := &rtsp.TrackInfo{
		Codec:     "H264",
		ClockRate: 90000,
		IsVideo:   true,
		SPS:       []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2},
		PPS:       []byte{0x68, 0xce, 0x38, 0x80},
	}

	sw, err := NewSegmentWriter(path, video, nil)
	if err != nil {
		t.Fatalf("NewSegmentWriter: %v", err)
	}

	err = sw.WriteAudio(&rtp.Packet{Payload: []byte{0xFF}})
	if err != nil {
		t.Errorf("WriteAudio should be no-op without audio track, got: %v", err)
	}

	_, err = sw.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestSegmentWriter_WithAudio(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "av.mp4")

	video := &rtsp.TrackInfo{
		Codec:     "H264",
		ClockRate: 90000,
		IsVideo:   true,
		SPS:       []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2},
		PPS:       []byte{0x68, 0xce, 0x38, 0x80},
	}
	audio := &rtsp.TrackInfo{
		Codec:     "AAC",
		ClockRate: 48000,
	}

	sw, err := NewSegmentWriter(path, video, audio)
	if err != nil {
		t.Fatalf("NewSegmentWriter: %v", err)
	}

	if !sw.hasAudio {
		t.Error("expected hasAudio to be true")
	}

	_, err = sw.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestSegmentWriter_NilTrack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nil.mp4")

	sw, err := NewSegmentWriter(path, nil, nil)
	if err != nil {
		t.Fatalf("NewSegmentWriter: %v", err)
	}

	err = sw.WriteVideo(&rtp.Packet{Payload: []byte{0x65, 0x88}})
	if err != nil {
		t.Errorf("WriteVideo with nil track: %v", err)
	}

	_, err = sw.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestSegmentWriter_WaitsForKeyframe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keyframe.mp4")

	video := &rtsp.TrackInfo{
		Codec:     "H264",
		ClockRate: 90000,
		IsVideo:   true,
		SPS:       []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2},
		PPS:       []byte{0x68, 0xce, 0x38, 0x80},
	}

	sw, err := NewSegmentWriter(path, video, nil)
	if err != nil {
		t.Fatalf("NewSegmentWriter: %v", err)
	}
	defer sw.Close()

	if sw.initWritten {
		t.Error("init should not be written before first keyframe")
	}
}

func TestIsRandomAccess(t *testing.T) {
	idr := [][]byte{{0x65, 0x88, 0x00}}
	if !h264.IsRandomAccess(idr) {
		t.Error("expected IDR to be random access")
	}

	nonIdr := [][]byte{{0x41, 0x9a, 0x00}}
	if h264.IsRandomAccess(nonIdr) {
		t.Error("expected non-IDR to not be random access")
	}
}

// h264TestPacket builds a single-NAL H264 RTP packet (packetization-mode 1).
// nal 0x65 = IDR keyframe, 0x41 = non-IDR slice.
func h264TestPacket(seq uint16, ts uint32, nal byte) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version: 2, PayloadType: 96, Marker: true,
			SequenceNumber: seq, Timestamp: ts, SSRC: 0xABCD,
		},
		Payload: []byte{nal, 0x88, 0x84, 0x00, 0x01},
	}
}

func newTestVideoWriter(t *testing.T, name string) *SegmentWriter {
	t.Helper()
	video := &rtsp.TrackInfo{
		Codec: "H264", ClockRate: 90000, IsVideo: true,
		SPS: []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2},
		PPS: []byte{0x68, 0xce, 0x38, 0x80},
	}
	sw, err := NewSegmentWriter(filepath.Join(t.TempDir(), name), video, nil)
	if err != nil {
		t.Fatalf("NewSegmentWriter: %v", err)
	}
	return sw
}

// A video RTP timestamp jump >= 2s must surface as ErrTimestampGap instead of
// being silently replaced with a ~33ms duration: the substitution compresses
// the gap and desyncs wall-clock time from media time for the rest of the
// file, which breaks wall-time seeking in playback.
func TestSegmentWriter_TimestampGapReturnsError(t *testing.T) {
	sw := newTestVideoWriter(t, "gap.mp4")
	defer sw.Close()

	if err := sw.WriteVideo(h264TestPacket(1, 0, 0x65)); err != nil {
		t.Fatalf("keyframe: %v", err)
	}
	if err := sw.WriteVideo(h264TestPacket(2, 3000, 0x41)); err != nil {
		t.Fatalf("frame: %v", err)
	}
	dtsBefore := sw.videoDTS

	// 3 seconds of missing stream (> the 2s plausibility bound). The frame
	// before the gap is written with the duration of the frame before it; the
	// gap adds no media time.
	err := sw.WriteVideo(h264TestPacket(3, 3000+3*90000, 0x41))
	if !errors.Is(err, ErrTimestampGap) {
		t.Fatalf("got err %v, want ErrTimestampGap", err)
	}
	if got, want := sw.videoDTS-dtsBefore, uint64(3000); got != want {
		t.Errorf("the gap added %d ticks of media, want the one frame before it (%d)", got, want)
	}

	// A backwards jump (camera clock reset) is a discontinuity too.
	err = sw.WriteVideo(h264TestPacket(4, 0, 0x41))
	if !errors.Is(err, ErrTimestampGap) {
		t.Fatalf("backwards jump: got err %v, want ErrTimestampGap", err)
	}
}

// Close must report the media duration actually written, not wall-clock age:
// the recording DB derives EndTime from it, and playback maps wall instants to
// media offsets by subtracting StartTime.
func TestSegmentWriter_CloseReturnsMediaDuration(t *testing.T) {
	sw := newTestVideoWriter(t, "mediadur.mp4")

	// 5 frames at 30fps: 5 * 3000 ticks = 15000 ticks = 166.66ms of media.
	for i := 0; i < 5; i++ {
		nal := byte(0x41)
		if i == 0 {
			nal = 0x65
		}
		if err := sw.WriteVideo(h264TestPacket(uint16(i+1), uint32(i*3000), nal)); err != nil {
			t.Fatalf("WriteVideo[%d]: %v", i, err)
		}
	}

	// Let wall time diverge from media time before closing.
	time.Sleep(300 * time.Millisecond)

	dur, err := sw.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	want := time.Duration(15000) * time.Second / 90000
	if dur != want {
		t.Errorf("duration = %v, want media duration %v (wall time must not leak in)", dur, want)
	}
}

// StartTime must reflect the first written sample (the keyframe that opens the
// file), not the moment the file was created: the writer waits for a keyframe
// before writing anything.
func TestSegmentWriter_StartTimeIsFirstSample(t *testing.T) {
	sw := newTestVideoWriter(t, "starttime.mp4")
	defer sw.Close()

	created := time.Now()
	time.Sleep(60 * time.Millisecond)

	before := time.Now()
	if err := sw.WriteVideo(h264TestPacket(1, 0, 0x65)); err != nil {
		t.Fatalf("keyframe: %v", err)
	}
	after := time.Now()

	st := sw.StartTime()
	if st.Before(before) || st.After(after) {
		t.Errorf("StartTime = %v, want within [%v, %v] (first sample, not creation %v)",
			st, before, after, created)
	}
}

func TestSegmentWriter_Duration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dur.mp4")

	video := &rtsp.TrackInfo{
		Codec:     "H264",
		ClockRate: 90000,
		IsVideo:   true,
		SPS:       []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2},
		PPS:       []byte{0x68, 0xce, 0x38, 0x80},
	}

	sw, err := NewSegmentWriter(path, video, nil)
	if err != nil {
		t.Fatalf("NewSegmentWriter: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	dur, err := sw.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if dur < 50*time.Millisecond {
		t.Errorf("duration = %v, expected >= 50ms", dur)
	}
}

// partialGOPWarning is the message SegmentWriter logs when it had to cut a GOP
// short.
const partialGOPWarning = "no keyframe within the expected interval, wrote a partial GOP"

// writeGOPs feeds the writer whole GOPs of the given length at 20 frames a
// second, and one more keyframe to end the last.
func writeGOPs(t *testing.T, sw *SegmentWriter, gops, framesPerGOP int) {
	t.Helper()
	const frame = 4500
	total := gops*framesPerGOP + 1
	for i := 0; i < total; i++ {
		nal := byte(0x41)
		if i%framesPerGOP == 0 {
			nal = 0x65
		}
		if err := sw.WriteVideo(h264TestPacket(uint16(i+1), uint32(i*frame), nal)); err != nil {
			t.Fatalf("WriteVideo[%d]: %v", i, err)
		}
	}
}

// A camera whose keyframe interval equals the part ceiling fills the buffer to
// the ceiling on the last frame of every GOP. The part written there is the
// whole GOP and the keyframe follows on time, so nothing is reported.
func TestSegmentWriter_AKeyframeIntervalAtTheCeilingIsNotReported(t *testing.T) {
	logs := countLogMessages(t)
	sw := newTestVideoWriter(t, "gop-at-ceiling.mp4")
	writeGOPs(t, sw, 3, 20*maxPendingPartSeconds)
	if _, err := sw.Close(); err != nil {
		t.Fatal(err)
	}
	if n := logs.count(partialGOPWarning); n != 0 {
		t.Errorf("%d warnings for a stream whose keyframes arrive on time", n)
	}
	if sw.seqNum != 4 {
		t.Errorf("wrote %d parts, want one per GOP and the closing keyframe (4)", sw.seqNum)
	}
}

// A stream whose keyframes are further apart than the ceiling has its GOPs cut
// into parts. That is reported once per segment, not at every GOP.
func TestSegmentWriter_ALateKeyframeIsReportedOncePerSegment(t *testing.T) {
	logs := countLogMessages(t)
	sw := newTestVideoWriter(t, "late-keyframe.mp4")
	writeGOPs(t, sw, 3, 20*maxPendingPartSeconds+10)
	if _, err := sw.Close(); err != nil {
		t.Fatal(err)
	}
	if n := logs.count(partialGOPWarning); n != 1 {
		t.Errorf("%d warnings, want one for the segment", n)
	}
	if got := logs.lastAttr(partialGOPWarning, "samples"); got != int64(20*maxPendingPartSeconds) {
		t.Errorf("warning reports %v samples, want the part that was cut (%d)", got, 20*maxPendingPartSeconds)
	}
}
