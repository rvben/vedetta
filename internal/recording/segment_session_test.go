package recording

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/rvben/vedetta/internal/config"
	"github.com/rvben/vedetta/internal/rtsp"
	"github.com/rvben/vedetta/internal/storage"
)

// sessionTestRig wires a SegmentRecorder to a hub source whose tracks are
// preset, so recordLoop proceeds without a live RTSP server.
type sessionTestRig struct {
	sr  *SegmentRecorder
	src *rtsp.Source
	url string
}

func newSessionTestRig(t *testing.T, ctx context.Context) *sessionTestRig {
	t.Helper()
	base := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := storage.New(dbPath)
	if err != nil {
		t.Fatalf("create db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	hub := rtsp.NewHub(ctx)
	sr := NewSegmentRecorder(config.RecordingConfig{Path: base, SegmentLength: time.Minute}, db, hub)
	// The default 256 MiB floor would pause recording on a nearly-full dev
	// volume and make this test measure free space instead of session
	// bookkeeping.
	sr.Disk().SetThreshold(1, nil)

	url := "rtsp://127.0.0.1:9/session-test"
	src := hub.GetOrCreate(url)
	src.SetVideoTrack(&rtsp.TrackInfo{
		Codec:     "H264",
		ClockRate: 90000,
		IsVideo:   true,
		SPS:       []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2},
		PPS:       []byte{0x68, 0xce, 0x38, 0x80},
	})
	return &sessionTestRig{sr: sr, src: src, url: url}
}

func (r *sessionTestRig) waitConsumers(t *testing.T, want int, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if r.src.ConsumerCount() == want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// Two events that overlap on one camera each call StartTemporaryRecording, which
// lands in StartRecording. Without a per-camera session that reference counts
// its callers, the second call attaches a second RecordingConsumer to the same
// source. Both consumers derive the same second-resolution filename, both
// os.Create it (truncating each other), and the path-keyed DB upsert collapses
// them into a single row describing a file that two writers are interleaving.
func TestSegmentRecorder_OverlappingEventsRecordOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rig := newSessionTestRig(t, ctx)

	ctxA, cancelA := context.WithCancel(ctx)
	defer cancelA()
	ctxB, cancelB := context.WithCancel(ctx)
	defer cancelB()

	// Two events on the same camera within the same second.
	rig.sr.StartRecording(ctxA, "front_door", rig.url)
	rig.sr.StartRecording(ctxB, "front_door", rig.url)

	if !rig.waitConsumers(t, 1, 2*time.Second) {
		t.Fatalf("recording never attached exactly one consumer: source has %d", rig.src.ConsumerCount())
	}
	// Give a second consumer time to attach if the bug is present.
	time.Sleep(300 * time.Millisecond)

	if n := rig.src.ConsumerCount(); n != 1 {
		t.Fatalf("source has %d recording consumers for one camera, want 1: two writers share one segment file and truncate each other", n)
	}
	rig.sr.mu.Lock()
	n := len(rig.sr.consumers)
	rig.sr.mu.Unlock()
	if n != 1 {
		t.Fatalf("recorder tracks %d consumers for one camera, want 1", n)
	}

	// Open a segment and confirm exactly one file is being written.
	rig.src.SimulateVideoRTPForTest(&rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 96, Marker: true, Timestamp: 0, SSRC: 0xABCD},
		Payload: []byte{0x65, 0x88, 0x84, 0x00, 0x01},
	})
	deadline := time.Now().Add(2 * time.Second)
	var open []CurrentSegment
	for time.Now().Before(deadline) {
		open = rig.sr.CurrentSegmentPaths()
		if len(open) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(open) != 1 {
		t.Fatalf("%d open segment files for one camera, want 1: %v", len(open), open)
	}
}

// When the first of two overlapping events ends, recording must continue for
// the second and remain stoppable. The unfixed code overwrote the camera's
// cancel func with the second recorder's, then had the first recorder's
// deferred cleanup delete that entry, so StopRecording silently became a no-op
// and the surviving recorder ran until process exit.
func TestSegmentRecorder_SecondEventKeepsRecordingAndStopStillWorks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rig := newSessionTestRig(t, ctx)

	ctxA, cancelA := context.WithCancel(ctx)
	defer cancelA()
	ctxB, cancelB := context.WithCancel(ctx)
	defer cancelB()

	rig.sr.StartRecording(ctxA, "front_door", rig.url)
	rig.sr.StartRecording(ctxB, "front_door", rig.url)
	if !rig.waitConsumers(t, 1, 2*time.Second) {
		t.Fatalf("recording never attached exactly one consumer: source has %d", rig.src.ConsumerCount())
	}

	// First event ends; the second still holds the camera.
	cancelA()
	time.Sleep(300 * time.Millisecond)
	if n := rig.src.ConsumerCount(); n != 1 {
		t.Fatalf("after the first event ended the camera has %d consumers, want 1 (recording must continue for the second event)", n)
	}

	// The camera must still be stoppable: the API stop path and the retention
	// paths both go through StopRecording.
	rig.sr.StopRecording("front_door")
	if !rig.waitConsumers(t, 0, 3*time.Second) {
		t.Fatalf("StopRecording did not stop the camera: source still has %d consumers", rig.src.ConsumerCount())
	}
}
