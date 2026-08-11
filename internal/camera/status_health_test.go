package camera

import (
	"context"
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/liberrors"

	"github.com/rvben/vedetta/internal/rtsp"
)

const testSourceURL = "rtsp://198.51.100.20:554/live0"

// seedOnDemandSource marks cam as on-demand and wires it to a hub holding a
// Source for testSourceURL that has already recorded n failed attempts with
// err. Nothing is dialled.
func seedOnDemandSource(t *testing.T, cam *Camera, n int, err error) {
	t.Helper()
	hub := rtsp.NewHub(context.Background())
	t.Cleanup(hub.Close)
	src := rtsp.NewSource(testSourceURL)
	hub.SetSourceForTest(testSourceURL, src)
	for range n {
		src.SimulateAttemptForTest(err)
	}
	cam.mu.Lock()
	cam.hub = hub
	cam.config.URL = testSourceURL
	cam.config.OnDemand = true
	cam.mu.Unlock()
}

// An on-demand camera reports no frames whether it is asleep or misconfigured,
// so Sleeping cannot be derived from frame arrival alone. Deriving it that way
// is what let a wrong password on a battery camera sit behind a badge saying
// the camera was merely resting, with nothing in the logs or metrics to
// contradict it.
func TestStatusOnDemandFaultIsNotSleeping(t *testing.T) {
	asleep := NewTestCamera("battery")
	seedOnDemandSource(t, asleep, 5, liberrors.ErrClientBadStatusCode{
		Code: base.StatusNotFound, Message: "Stream Not Found",
	})
	st := asleep.Status()
	if !st.Sleeping {
		t.Errorf("sleeping = false for an on-demand camera whose stream is unpublished, want true: that is its resting state")
	}
	if st.StreamError != "" {
		t.Errorf("stream_error = %q for a resting camera, want empty: an operator must not be sent after a fault that does not exist", st.StreamError)
	}

	broken := NewTestCamera("battery-broken")
	seedOnDemandSource(t, broken, 5, liberrors.ErrClientBadStatusCode{
		Code: base.StatusUnauthorized, Message: "Unauthorized",
	})
	st = broken.Status()
	if st.Sleeping {
		t.Errorf("sleeping = true for an on-demand camera whose credentials are rejected, want false: this is an outage reported as a nap")
	}
	if st.Online {
		t.Errorf("online = true for a camera that never connected, want false")
	}
	if st.StreamError == "" {
		t.Errorf("stream_error empty for a rejected connection, want the reason the camera cannot be reached")
	}
}

// A camera that has never connected has no last-connected time. Reporting the
// zero value would give consumers a timestamp in year one, whose age reads as a
// plausible-looking two millennia rather than as missing data.
func TestStatusLastConnectedAbsentUntilFirstConnection(t *testing.T) {
	cam := NewTestCamera("battery")
	seedOnDemandSource(t, cam, 1, liberrors.ErrClientBadStatusCode{
		Code: base.StatusNotFound, Message: "Stream Not Found",
	})
	if got := cam.Status().LastConnected; !got.IsZero() {
		t.Errorf("last_connected = %v for a camera that has never connected, want the zero value so the field is omitted", got)
	}
}

// A camera with no Source yet (nothing has opened the stream) must not be
// reported as faulted. Nothing has tried, which is a different fact from
// having tried and failed.
func TestStatusNoSourceIsNotFaulted(t *testing.T) {
	cam := NewTestCamera("battery")
	cam.mu.Lock()
	cam.config.OnDemand = true
	cam.mu.Unlock()

	st := cam.Status()
	if st.StreamError != "" {
		t.Errorf("stream_error = %q with no source created yet, want empty", st.StreamError)
	}
	if !st.Sleeping {
		t.Errorf("sleeping = false for an on-demand camera with no source yet, want true")
	}
}
