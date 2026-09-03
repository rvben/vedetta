package stream

import (
	"context"
	"testing"

	"github.com/rvben/vedetta/internal/rtsp"
)

func mseRig(t *testing.T) (*MSEManager, *rtsp.Source, string) {
	t.Helper()
	silenceRepublisherLogs(t)

	const url = "rtsp://192.0.2.20:554/main"
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

	m := NewMSEManager(hub, nil, nil)
	t.Cleanup(m.Close)
	return m, source, url
}

// A hand-built viewer. close() only closes the done channel, so a viewer needs
// no WebSocket to observe whether the manager disconnected it.
func newTestViewer() *mseClient {
	return &mseClient{ch: make(chan []byte, 1), done: make(chan struct{})}
}

func viewerWasDisconnected(c *mseClient) bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// getOrCreateConsumer replaces a consumer the Source detached, which is right,
// but the viewers already watching through it were left behind: still holding
// an open WebSocket, still inside the old consumer's client list, and attached
// to nothing. writePump keeps pinging, so the connection never drops, and the
// browser has no reason to reconnect. The picture simply freezes for as long as
// the tab stays open.
//
// Closing them turns that into the one thing the client already handles: an
// onclose, which reconnects and lands on the live consumer.
func TestReplacingADetachedConsumerDisconnectsItsViewers(t *testing.T) {
	m, source, url := mseRig(t)

	first := m.getOrCreateConsumer("cam1", url)
	viewer := newTestViewer()
	first.addClient(viewer)

	// The state Source.deliver leaves behind after recovering a consumer panic.
	source.RemoveConsumer(first)

	second := m.getOrCreateConsumer("cam1", url)
	if second == first {
		t.Fatal("the detached consumer was reused, so this test proves nothing")
	}

	if !viewerWasDisconnected(viewer) {
		t.Fatal("the viewer was left on the replaced consumer: its WebSocket stays open " +
			"and its video is frozen for as long as the tab is")
	}
}

// The accepting bound: a live consumer is returned as it is, and its viewers
// keep watching. Disconnecting on every request would drop every viewer each
// time another one connects.
func TestASecondViewerDoesNotDisconnectTheFirst(t *testing.T) {
	m, _, url := mseRig(t)

	first := m.getOrCreateConsumer("cam1", url)
	viewer := newTestViewer()
	first.addClient(viewer)

	if again := m.getOrCreateConsumer("cam1", url); again != first {
		t.Fatal("an attached consumer was replaced")
	}
	if viewerWasDisconnected(viewer) {
		t.Fatal("an existing viewer was disconnected by another viewer connecting")
	}
}
