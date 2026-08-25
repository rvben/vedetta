package rtsp

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer is a mutex-guarded log sink: the connect loop logs from its own
// goroutine while the test reads the buffer.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureSlog routes the default logger into a buffer for the test's duration.
func captureSlog(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return buf
}

// TestFailingSourceLogsOnceNotEveryAttempt pins the property that keeps the
// error log readable: an unreachable camera reconnects forever, and logging
// every attempt made two long-dead cameras 170,000 lines and the bulk of a
// production error log, burying the faults worth reading.
//
// The attempt count is the control. Asserting only "one line" would pass
// trivially if the source had made a single attempt in the observation window,
// which is also what a broken retry loop looks like, so the test would report
// success for the opposite defect.
func TestFailingSourceLogsOnceNotEveryAttempt(t *testing.T) {
	buf := captureSlog(t)
	srv := newRTSPProbeServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go NewSourceWithTransport(srv.url(), "tcp").Connect(ctx)

	// Backoff starts at 5s and jitter halves a wait at most, so gaps run
	// [2.5s, 5s] then [3.75s, 7.5s]. Observing 13s guarantees at least three
	// attempts, and therefore at least three log lines before this change.
	const observe = 13 * time.Second
	time.Sleep(observe)
	cancel()

	attempts := len(srv.offsets())
	if attempts < 3 {
		t.Fatalf("source made %d attempts in %s; too few for the assertion below to "+
			"distinguish throttling from a stalled retry loop", attempts, observe)
	}

	lines := strings.Count(buf.String(), "RTSP connection error, reconnecting")
	if lines != 1 {
		t.Errorf("got %d error lines for %d failed attempts, want exactly 1; "+
			"a permanently unreachable camera must not repeat itself every retry\n%s",
			lines, attempts, buf)
	}
}

// The throttle must not silence a camera that has just gone down: the first
// failure after any successful connection is reported at once, and only repeats
// are held back. Without the re-arm on success, a fault appearing minutes after
// a good connection would wait out an interval that started while the camera
// was still working.
func TestFailureLogThrottleReArmsAfterSuccess(t *testing.T) {
	src := NewSourceWithTransport("rtsp://192.0.2.10:554/stream", "tcp")

	if !src.shouldLogFailure() {
		t.Fatal("first failure suppressed; a camera going down must be reported immediately")
	}
	if src.shouldLogFailure() {
		t.Error("consecutive failure logged; repeats within the interval must be held back")
	}

	// A pass that connected after it began is a success, which clears the stamp.
	attemptStart := time.Now()
	src.mu.Lock()
	src.lastConnected = attemptStart.Add(time.Millisecond)
	src.mu.Unlock()
	src.recordAttempt(attemptStart, nil)

	if !src.shouldLogFailure() {
		t.Error("failure after a good connection suppressed; the throttle must re-arm on success")
	}

	// Once the interval has elapsed the condition is worth restating, so an
	// operator reading the log can tell an ongoing fault from a resolved one.
	src.mu.Lock()
	src.lastWarn = time.Now().Add(-failureLogInterval - time.Second)
	src.mu.Unlock()

	if !src.shouldLogFailure() {
		t.Error("failure suppressed after the interval elapsed; an ongoing fault must restate itself")
	}
}
