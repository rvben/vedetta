package media

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"

	"github.com/rvben/vedetta/internal/rtsp"
)

// messageCounter is a slog.Handler that counts records by message.
type messageCounter struct {
	mu     sync.Mutex
	counts map[string]int
	last   map[string]map[string]any
}

func (c *messageCounter) Enabled(context.Context, slog.Level) bool { return true }
func (c *messageCounter) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *messageCounter) WithGroup(string) slog.Handler            { return c }
func (c *messageCounter) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]any{}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[r.Message]++
	c.last[r.Message] = attrs
	return nil
}

func (c *messageCounter) count(msg string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[msg]
}

func (c *messageCounter) lastAttr(msg, key string) any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last[msg][key]
}

func countLogMessages(t *testing.T) *messageCounter {
	t.Helper()
	counter := &messageCounter{counts: map[string]int{}, last: map[string]map[string]any{}}
	previous := slog.Default()
	slog.SetDefault(slog.New(counter))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return counter
}

const (
	msgWriteFailed = "write failed"
	msgPausing     = "repeated write failures, pausing recording"
	msgDiskResumed = "recording resumed, disk space recovered"
	msgResumed     = "recording resumed, writes succeed again"
)

// A volume that stays out fails every retry the same way. The outage is
// reported when it starts, at intervals while it lasts and when it ends, not
// at each retry.
func TestRecordingConsumer_WriteOutageIsReportedOnceUntilItEnds(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can create files in a read-only directory")
	}
	segDir := filepath.Join(t.TempDir(), "segments")
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(segDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(segDir, 0o755) })

	video := &rtsp.TrackInfo{
		Codec: "H264", ClockRate: 90000, IsVideo: true,
		SPS: []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2},
		PPS: []byte{0x68, 0xce, 0x38, 0x80},
	}
	rc := NewRecordingConsumer(segDir, "test-cam", time.Minute, video, nil, testDiskNoFloor(t), nil)
	defer rc.Close()
	logs := countLogMessages(t)

	var ts uint32
	frame := func() {
		ts += 3000
		rc.dispatch(rtpMsg{video: true, pkt: &rtp.Packet{
			Header:  rtp.Header{PayloadType: 96, Timestamp: ts},
			Payload: []byte{0x65, 0x00, 0x01},
		}})
	}
	// retry lets the pause run out, as diskPauseRetryInterval does, and feeds
	// the frames of the attempt that follows.
	retry := func() {
		rc.mu.Lock()
		rc.pausedSince = time.Now().Add(-diskPauseRetryInterval)
		rc.mu.Unlock()
		for range 4 {
			frame()
		}
	}

	for range 3 {
		frame()
	}
	if !rc.Paused() {
		t.Fatal("recording is not paused after three failed writes")
	}
	if got := logs.count(msgWriteFailed); got != 2 {
		t.Errorf("start of the outage logged %q %d times, want 2", msgWriteFailed, got)
	}
	if got := logs.count(msgPausing); got != 1 {
		t.Errorf("start of the outage logged %q %d times, want 1", msgPausing, got)
	}

	for range 20 {
		retry()
		if !rc.Paused() {
			t.Fatal("recording is not paused again after a failed retry")
		}
	}
	if got := logs.count(msgWriteFailed); got != 2 {
		t.Errorf("%q logged %d times after 20 failed retries, want still 2", msgWriteFailed, got)
	}
	if got := logs.count(msgPausing); got != 1 {
		t.Errorf("%q logged %d times after 20 failed retries, want still 1", msgPausing, got)
	}
	if got := logs.count(msgDiskResumed); got != 0 {
		t.Errorf("a retry was logged %d times as %q", got, msgDiskResumed)
	}

	rc.mu.Lock()
	rc.outage.lastReport = time.Now().Add(-writeOutageReportInterval)
	rc.mu.Unlock()
	retry()
	if got := logs.count(msgPausing); got != 2 {
		t.Errorf("%q logged %d times once the report interval passed, want 2", msgPausing, got)
	}
	if got := logs.lastAttr(msgPausing, "retries_since_last_report"); got != int64(20) {
		t.Errorf("retries_since_last_report = %v, want 20", got)
	}

	if err := os.Chmod(segDir, 0o755); err != nil {
		t.Fatal(err)
	}
	retry()
	if rc.Paused() {
		t.Fatal("recording is still paused after the volume came back")
	}
	if got := logs.count(msgResumed); got != 1 {
		t.Errorf("the end of the outage logged %q %d times, want 1", msgResumed, got)
	}
	retry()
	if got := logs.count(msgResumed); got != 1 {
		t.Errorf("%q logged %d times, want it once per outage", msgResumed, got)
	}
}

// A volume can accept a new file and still refuse its contents. The writer
// buffers a GOP before it writes one, so the packets of a retry are accepted
// without touching the volume, and the outage has not ended until media
// reached the file.
func TestRecordingConsumer_WriteOutageEndsWhenMediaIsWritten(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can create files in a read-only directory")
	}
	segDir := filepath.Join(t.TempDir(), "segments")
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(segDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(segDir, 0o755) })

	video := &rtsp.TrackInfo{
		Codec: "H264", ClockRate: 90000, IsVideo: true,
		SPS: []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2},
		PPS: []byte{0x68, 0xce, 0x38, 0x80},
	}
	rc := NewRecordingConsumer(segDir, "test-cam", time.Minute, video, nil, testDiskNoFloor(t), nil)
	defer rc.Close()
	logs := countLogMessages(t)

	var ts uint32
	frame := func() {
		ts += 3000
		rc.dispatch(rtpMsg{video: true, pkt: &rtp.Packet{
			Header:  rtp.Header{PayloadType: 96, Timestamp: ts},
			Payload: []byte{0x65, 0x00, 0x01},
		}})
	}
	endPause := func() {
		rc.mu.Lock()
		rc.pausedSince = time.Now().Add(-diskPauseRetryInterval)
		rc.mu.Unlock()
	}

	for range 3 {
		frame()
	}
	if !rc.Paused() {
		t.Fatal("recording is not paused after three failed writes")
	}

	// The directory takes a file again, but the file takes no data.
	if err := os.Chmod(segDir, 0o755); err != nil {
		t.Fatal(err)
	}
	endPause()
	opened := false
	for range 2 {
		frame()
		rc.mu.Lock()
		if rc.writer != nil && !opened {
			opened = true
			_ = rc.writer.f.Close()
		}
		rc.mu.Unlock()
		if opened {
			break
		}
	}
	if !opened {
		t.Fatal("the retry did not open a segment")
	}
	if got := logs.count(msgResumed); got != 0 {
		t.Fatalf("%q logged %d times for a packet that was only buffered", msgResumed, got)
	}
	for range 8 {
		frame()
	}
	if got := logs.count(msgResumed); got != 0 {
		t.Errorf("%q logged %d times while no media reached the file", msgResumed, got)
	}
	if !rc.Paused() {
		t.Error("recording is not paused again although every write of the retry failed")
	}
	if got := logs.count(msgWriteFailed); got != 2 {
		t.Errorf("%q logged %d times, want the 2 from the start of the outage", msgWriteFailed, got)
	}
	if got := logs.count(msgPausing); got != 1 {
		t.Errorf("%q logged %d times, want 1 for one outage", msgPausing, got)
	}

	endPause()
	for range 4 {
		frame()
	}
	if rc.Paused() {
		t.Fatal("recording is still paused after the volume came back")
	}
	if got := logs.count(msgResumed); got != 1 {
		t.Errorf("the end of the outage logged %q %d times, want 1", msgResumed, got)
	}
}

// One failed write that the next one recovers from is an error worth its
// line, and not an outage that ends with an announcement.
func TestWriteOutage_SingleFailureIsLoggedAndEndsQuietly(t *testing.T) {
	var o writeOutage
	now := time.Now()
	if !o.failure(now) {
		t.Error("a first failure was not logged")
	}
	if _, wasPaused := o.recovered(now.Add(time.Second)); wasPaused {
		t.Error("a failure that never paused recording was announced as a recovery")
	}
	if !o.failure(now.Add(time.Minute)) {
		t.Error("a failure after a recovery was not logged")
	}
}

func TestWriteOutage_NextOutageIsReportedAtOnce(t *testing.T) {
	var o writeOutage
	now := time.Now()
	o.failure(now)
	if report, _ := o.pause(now); !report {
		t.Fatal("the first pause was not reported")
	}
	if lasted, wasPaused := o.recovered(now.Add(time.Minute)); !wasPaused || lasted != time.Minute {
		t.Fatalf("recovered = %v, %v; want 1m0s, true", lasted, wasPaused)
	}
	later := now.Add(2 * time.Minute)
	if !o.failure(later) {
		t.Error("the first failure of the next outage was not logged")
	}
	if report, _ := o.pause(later); !report {
		t.Error("the next outage's first pause fell inside the previous outage's interval and was not reported")
	}
}

// outageTestConsumer returns a consumer recording into segDir and a function
// that feeds it one single-packet frame, a keyframe or not, after advance
// ticks of the 90 kHz clock.
func outageTestConsumer(t *testing.T, segDir string) (*RecordingConsumer, func(keyframe bool, advance uint32)) {
	t.Helper()
	video := &rtsp.TrackInfo{
		Codec: "H264", ClockRate: 90000, IsVideo: true,
		SPS: []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2},
		PPS: []byte{0x68, 0xce, 0x38, 0x80},
	}
	rc := NewRecordingConsumer(segDir, "test-cam", time.Minute, video, nil, testDiskNoFloor(t), nil)
	t.Cleanup(rc.Close)
	var ts uint32
	return rc, func(keyframe bool, advance uint32) {
		ts += advance
		nal := byte(0x41)
		if keyframe {
			nal = 0x65
		}
		rc.dispatch(rtpMsg{video: true, pkt: &rtp.Packet{
			Header:  rtp.Header{PayloadType: 96, Timestamp: ts},
			Payload: []byte{nal, 0x00, 0x01},
		}})
	}
}

// A segment that ends before its first GOP is complete writes that GOP when
// it closes. A stream that stalls once per GOP never flushes one in an open
// segment, and its outage has ended all the same once a close put media in a
// file.
func TestRecordingConsumer_WriteOutageEndsWhenARotationWritesTheMedia(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can create files in a read-only directory")
	}
	segDir := filepath.Join(t.TempDir(), "segments")
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(segDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(segDir, 0o755) })
	rc, frame := outageTestConsumer(t, segDir)
	logs := countLogMessages(t)

	for range 3 {
		frame(true, 3000)
	}
	if !rc.Paused() {
		t.Fatal("recording is not paused after three failed writes")
	}
	if err := os.Chmod(segDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rc.mu.Lock()
	rc.pausedSince = time.Now().Add(-diskPauseRetryInterval)
	rc.mu.Unlock()

	// The retry opens a segment, which waits for a keyframe.
	frame(false, 3000)
	frame(true, 3000)
	for range 3 {
		frame(false, 3000)
	}
	rc.mu.Lock()
	open, flushed := rc.writer != nil, time.Duration(0)
	if open {
		flushed = rc.writer.FlushedDuration()
	}
	rc.mu.Unlock()
	if !open || flushed != 0 {
		t.Fatalf("open = %v, flushed = %v; the test needs an open segment whose GOP is still buffered", open, flushed)
	}
	if got := logs.count(msgResumed); got != 0 {
		t.Fatalf("%q logged %d times while the GOP was only buffered", msgResumed, got)
	}

	// The stream stalls for a minute and resumes on a keyframe.
	frame(true, 60*90000)
	frame(false, 3000)
	if got := logs.count(msgResumed); got != 1 {
		t.Errorf("%q logged %d times after the rotation wrote the GOP, want 1", msgResumed, got)
	}
	if got := logs.count(msgWriteFailed); got != 2 {
		t.Errorf("%q logged %d times, want the 2 from the start of the outage", msgWriteFailed, got)
	}
}

// A volume can fail under a segment that already holds media. The frames
// between two GOP writes are only buffered, and accepting them is no sign
// that the volume is back: three failed GOP writes in a row pause recording.
func TestRecordingConsumer_BufferedFramesDoNotEndAWriteOutage(t *testing.T) {
	rc, frame := outageTestConsumer(t, t.TempDir())
	logs := countLogMessages(t)

	gop := func() {
		frame(true, 3000)
		frame(false, 3000)
		frame(false, 3000)
	}
	gop()
	gop()
	gop()
	rc.mu.Lock()
	if rc.writer == nil || rc.writer.FlushedDuration() == 0 {
		rc.mu.Unlock()
		t.Fatal("the test needs an open segment that already holds media")
	}
	_ = rc.writer.f.Close()
	rc.mu.Unlock()

	for range 3 {
		if rc.Paused() {
			t.Fatal("recording paused before three GOP writes had failed")
		}
		gop()
	}
	// The frame after the third GOP's keyframe completes that keyframe.
	frame(false, 3000)
	if !rc.Paused() {
		t.Errorf("recording is not paused after three failed GOP writes; %q logged %d times",
			msgWriteFailed, logs.count(msgWriteFailed))
	}
	if got := logs.count(msgResumed); got != 0 {
		t.Errorf("%q logged %d times while every write failed", msgResumed, got)
	}
}

// The last frame before a stall is written when the stall is noticed. A
// volume that refuses it has failed a write, whatever the rotation that
// follows does.
func TestRecordingConsumer_WriteFailureAtATimestampGapIsReported(t *testing.T) {
	rc, frame := outageTestConsumer(t, t.TempDir())
	logs := countLogMessages(t)

	frame(true, 3000)
	frame(false, 3000)
	// A keyframe ends the GOP before it, so writing it writes to the file.
	frame(true, 3000)
	rc.mu.Lock()
	if rc.writer == nil {
		rc.mu.Unlock()
		t.Fatal("no segment is open")
	}
	_ = rc.writer.f.Close()
	rc.mu.Unlock()

	frame(true, 60*90000)
	if got := logs.count(msgWriteFailed); got != 1 {
		t.Errorf("%q logged %d times for the frame the volume refused at the stall, want 1", msgWriteFailed, got)
	}
	rc.mu.Lock()
	writeErrors := rc.writeErrors
	rc.mu.Unlock()
	if writeErrors != 1 {
		t.Errorf("writeErrors = %d after one refused frame, want 1", writeErrors)
	}
}
