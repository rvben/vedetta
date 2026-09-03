package stream

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/pion/rtp"

	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/rtsp"
)

func silenceRepublisherLogs(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

// republisherRig starts a republisher for one camera whose source already has a
// video track, so Start attaches a consumer the way it does in production.
func republisherRig(t *testing.T, interval time.Duration) (*RTSPServer, *rtsp.Source, *cameraStream) {
	t.Helper()
	silenceRepublisherLogs(t)

	const url = "rtsp://192.0.2.10:554/main"

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	hub := rtsp.NewHub(ctx)

	source := rtsp.NewSource(url)
	source.SetVideoTrack(&rtsp.TrackInfo{
		Codec:       "H264",
		ClockRate:   90000,
		IsVideo:     true,
		PayloadType: 96,
		SPS:         []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2},
		PPS:         []byte{0x68, 0xce, 0x38, 0x80},
	})
	hub.SetSourceForTest(url, source)

	rs := NewRTSPServer(hub, config.RTSPServerConfig{Enabled: true}, nil,
		[]config.CameraConfig{{Name: "cam1", URL: url}})
	// A loopback port, and no UDP listeners, so parallel packages do not fight
	// over the fixed ports the production config uses.
	rs.server.RTSPAddress = "127.0.0.1:0"
	rs.server.UDPRTPAddress = ""
	rs.server.UDPRTCPAddress = ""
	rs.reattachInterval = interval

	if err := rs.Start(); err != nil {
		t.Fatalf("start republisher: %v", err)
	}
	t.Cleanup(rs.Close)

	cs, ok := rs.cameras["cam1"]
	if !ok {
		t.Fatal("camera was not registered")
	}
	if source.ConsumerCount() != 1 {
		t.Fatalf("consumers after start = %d, want 1", source.ConsumerCount())
	}
	return rs, source, cs
}

func videoRTP(seq uint16) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version: 2, PayloadType: 96, SequenceNumber: seq,
			Timestamp: uint32(seq) * 3000, SSRC: 0x1234, Marker: true,
		},
		// A single NAL unit packet carrying a non-IDR slice.
		Payload: []byte{0x41, 0x9a, 0x00, 0x01},
	}
}

// Source.deliver detaches a consumer whose callback panicked, leaving the owner
// to reattach. The republisher never did. Its recovery on reconnect cannot
// help: initLateCamera re-initializes only while cs.stream is nil, and a
// detached consumer leaves the stream in place, so DESCRIBE keeps returning it
// and a client negotiates successfully onto a stream that is permanently
// silent. That is worse than an error, because nothing reports a fault.
func TestRepublishedStreamRecoversAfterTheSourceDetachesIt(t *testing.T) {
	rs, source, cs := republisherRig(t, 5*time.Millisecond)

	cs.mu.RLock()
	first := cs.consumer
	cs.mu.RUnlock()

	// The state deliver leaves behind after it recovers a consumer panic.
	source.RemoveConsumer(first)

	// The failure is invisible from the client's side: the stream is still
	// advertised, so nothing upstream will ever trigger a re-init.
	resp, stream, err := rs.OnDescribe(&gortsplib.ServerHandlerOnDescribeCtx{
		Path:    "/cam1",
		Request: &base.Request{},
	})
	if err != nil {
		t.Fatalf("OnDescribe: %v", err)
	}
	if resp.StatusCode != base.StatusOK || stream == nil {
		t.Fatalf("DESCRIBE after detach = %d (stream %v), want 200 with a stream: "+
			"the premise is that this failure is invisible to clients", resp.StatusCode, stream != nil)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if source.ConsumerCount() == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if source.ConsumerCount() != 1 {
		t.Fatalf("consumers = %d, want 1: the republished stream stays silent forever", source.ConsumerCount())
	}

	cs.mu.RLock()
	next := cs.consumer
	cs.mu.RUnlock()
	if next == first {
		t.Fatal("the detached consumer was re-registered: its decoder state is where the panic happened")
	}
	if !source.HasConsumer(next) {
		t.Fatal("cameraStream tracks a consumer the source does not have")
	}

	// The replacement has to actually publish, not merely be registered.
	source.SimulateVideoRTPForTest(videoRTP(1))
}

// The accepting bound: an attached consumer is left exactly as it is, so the
// check cannot churn a working stream. A rebuild would reset the H264
// depacketizer mid-GOP and drop frames for every connected client.
func TestRepublishedStreamSurvivesTicksWhileAttached(t *testing.T) {
	_, source, cs := republisherRig(t, 2*time.Millisecond)

	cs.mu.RLock()
	first := cs.consumer
	cs.mu.RUnlock()

	time.Sleep(60 * time.Millisecond)

	cs.mu.RLock()
	still := cs.consumer
	cs.mu.RUnlock()
	if still != first {
		t.Fatal("an attached consumer was replaced")
	}
	if source.ConsumerCount() != 1 {
		t.Fatalf("consumers = %d, want 1: the check registered a duplicate", source.ConsumerCount())
	}
}

// A camera whose source had no tracks at startup has no consumer at all. That
// is OnDescribe's late-init case, not a detach, and the check must leave it
// alone rather than treat nil as something to rebuild from.
func TestRepublisherIgnoresACameraThatWasNeverInitialized(t *testing.T) {
	silenceRepublisherLogs(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	hub := rtsp.NewHub(ctx)

	const url = "rtsp://192.0.2.11:554/main"
	hub.SetSourceForTest(url, rtsp.NewSource(url))

	rs := NewRTSPServer(hub, config.RTSPServerConfig{Enabled: true}, nil,
		[]config.CameraConfig{{Name: "late", URL: url}})
	rs.server.RTSPAddress = "127.0.0.1:0"
	rs.server.UDPRTPAddress = ""
	rs.server.UDPRTCPAddress = ""
	rs.reattachInterval = 2 * time.Millisecond

	if err := rs.Start(); err != nil {
		t.Fatalf("start republisher: %v", err)
	}
	t.Cleanup(rs.Close)

	cs := rs.cameras["late"]
	cs.mu.RLock()
	consumer := cs.consumer
	cs.mu.RUnlock()
	if consumer != nil {
		t.Fatal("a source with no tracks must not produce a consumer, or this test proves nothing")
	}

	// Long enough for many ticks. The failure mode is a panic on the reattach
	// goroutine, which takes the process down rather than failing a check.
	time.Sleep(60 * time.Millisecond)

	cs.mu.RLock()
	consumer = cs.consumer
	cs.mu.RUnlock()
	if consumer != nil {
		t.Fatal("the check invented a consumer for an uninitialized camera")
	}
}
