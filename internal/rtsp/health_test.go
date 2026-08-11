package rtsp

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/liberrors"
)

func unpublishedErr() error {
	return liberrors.ErrClientBadStatusCode{Code: base.StatusNotFound, Message: "Stream Not Found"}
}

func unauthorizedErr() error {
	return liberrors.ErrClientBadStatusCode{Code: base.StatusUnauthorized, Message: "Unauthorized"}
}

// A bridge answering "no stream at this path" is a battery camera between
// events. A bridge rejecting our credentials is a broken camera. Both deliver
// zero frames forever, so if the Source does not separate them here, nothing
// downstream can: the wrong password is reported as a healthy nap.
func TestSourceHealthSeparatesUnpublishedFromFault(t *testing.T) {
	asleep := NewSource("rtsp://198.51.100.20:554/live0")
	for range onDemandFaultThreshold + 2 {
		asleep.SimulateAttemptForTest(unpublishedErr())
	}
	h := asleep.Health()
	if !h.Unpublished {
		t.Errorf("Unpublished = false for a 404, want true")
	}
	if h.Faulted() {
		t.Errorf("Faulted = true for a camera whose stream is merely unpublished: that is its resting state, not a fault")
	}
	if h.ConsecutiveFailures != onDemandFaultThreshold+2 {
		t.Errorf("ConsecutiveFailures = %d, want %d", h.ConsecutiveFailures, onDemandFaultThreshold+2)
	}

	rejected := NewSource("rtsp://198.51.100.20:554/live0")
	for range onDemandFaultThreshold - 1 {
		rejected.SimulateAttemptForTest(unauthorizedErr())
	}
	if rejected.Health().Faulted() {
		t.Errorf("Faulted = true after %d failures, want false: a bridge reboot must not flap the camera between states",
			onDemandFaultThreshold-1)
	}
	rejected.SimulateAttemptForTest(unauthorizedErr())
	h = rejected.Health()
	if !h.Faulted() {
		t.Errorf("Faulted = false after %d rejected credentials, want true", onDemandFaultThreshold)
	}
	if h.Unpublished {
		t.Errorf("Unpublished = true for a 401, want false")
	}
	if !strings.Contains(h.LastError, "401") {
		t.Errorf("LastError = %q, want it to name the 401 so an operator can act on it", h.LastError)
	}
}

// A transport-level failure (refused, DNS, timeout) carries no RTSP status at
// all. It must land on the fault side, not be mistaken for a resting stream.
func TestSourceHealthTreatsTransportErrorsAsFault(t *testing.T) {
	src := NewSource("rtsp://198.51.100.20:554/live0")
	for range onDemandFaultThreshold {
		src.SimulateAttemptForTest(errors.New("dial tcp 198.51.100.20:554: connect: connection refused"))
	}
	h := src.Health()
	if h.Unpublished {
		t.Errorf("Unpublished = true for a refused connection, want false")
	}
	if !h.Faulted() {
		t.Errorf("Faulted = false for a refused connection, want true")
	}
}

// A source that connects and later drops has not failed to reach the camera.
// Counting that as a failure would let a working on-demand camera cross the
// fault threshold once per event and report itself broken between naps.
func TestSourceHealthConnectionResetsFailures(t *testing.T) {
	src := NewSource("rtsp://198.51.100.20:554/live0")
	for range onDemandFaultThreshold {
		src.SimulateAttemptForTest(unauthorizedErr())
	}
	if !src.Health().Faulted() {
		t.Fatalf("precondition: want Faulted before the connection")
	}

	start := time.Now().Add(-time.Second)
	src.mu.Lock()
	src.connected = true
	src.lastConnected = time.Now()
	src.mu.Unlock()
	src.recordAttempt(start, errors.New("EOF"))

	h := src.Health()
	if h.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d after a connection that dropped, want 0", h.ConsecutiveFailures)
	}
	if h.LastError != "" {
		t.Errorf("LastError = %q after a connection that dropped, want empty", h.LastError)
	}
	if h.LastConnected.IsZero() {
		t.Errorf("LastConnected is zero after a connection, want it stamped")
	}
	// Connected is still true here because only notifyDisconnect clears it;
	// Faulted must not fire while the source holds a live connection.
	if h.Faulted() {
		t.Errorf("Faulted = true while connected, want false")
	}
}

// Error strings from the client library quote the URL they were handed, which
// carries the camera password. That string reaches the log and the HTTP API.
func TestRedactCredentials(t *testing.T) {
	raw := "rtsp://user:s3cr3t@198.51.100.20:554/live0"
	msg := redactCredentials(fmt.Sprintf("dial %s: connection refused", raw), raw)
	if strings.Contains(msg, "s3cr3t") {
		t.Errorf("redacted message still contains the password: %q", msg)
	}
	if !strings.Contains(msg, "198.51.100.20") {
		t.Errorf("redacted message lost the host, leaving nothing to diagnose: %q", msg)
	}

	// The userinfo can also appear on its own, without the rest of the URL.
	msg = redactCredentials("auth failed for user:s3cr3t@198.51.100.20", raw)
	if strings.Contains(msg, "s3cr3t") {
		t.Errorf("bare userinfo not redacted: %q", msg)
	}

	// A URL with no credentials must survive untouched, or every message from a
	// credential-free camera would be mangled.
	plain := "rtsp://198.51.100.20:554/live0"
	if got := redactCredentials("bad status code: 404 (Stream Not Found)", plain); got != "bad status code: 404 (Stream Not Found)" {
		t.Errorf("message altered for a credential-free URL: %q", got)
	}
}
