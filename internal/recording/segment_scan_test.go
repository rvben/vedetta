package recording

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pion/rtp"
	"github.com/rvben/vedetta/internal/media"
	"github.com/rvben/vedetta/internal/rtsp"
	"github.com/rvben/vedetta/internal/storage"
)

// writeProbeableSegment writes a real fMP4 segment with the production writer,
// so the scan has a positive control that must be imported.
func writeProbeableSegment(t *testing.T, path string) {
	t.Helper()
	video := &rtsp.TrackInfo{
		Codec: "H264", ClockRate: 90000, IsVideo: true,
		SPS: []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2},
		PPS: []byte{0x68, 0xce, 0x38, 0x80},
	}
	sw, err := media.NewSegmentWriter(path, video, nil)
	if err != nil {
		t.Fatalf("NewSegmentWriter: %v", err)
	}
	for i := 0; i < 6; i++ {
		nal := byte(0x41)
		if i == 0 {
			nal = 0x65
		}
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version: 2, PayloadType: 96, Marker: true,
				SequenceNumber: uint16(i + 1), Timestamp: uint32(i * 3000), SSRC: 0xABCD,
			},
			Payload: []byte{nal, 0x88, 0x84, 0x00, 0x01},
		}
		if err := sw.WriteVideo(pkt); err != nil {
			t.Fatalf("write packet %d: %v", i, err)
		}
	}
	if _, err := sw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
}

// A file whose duration cannot be read is not a file of zero duration. Importing
// it with duration 0 writes StartTime == EndTime, which is indistinguishable
// from a genuine zero-length segment: the row overlaps no query range, so clip
// extraction silently skips the footage while retention keeps counting it.
func TestSegmentRecorder_UnreadableDurationIsNotImportedAsZero(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rig := newSessionTestRig(t, ctx)

	segDir := filepath.Join(t.TempDir(), "front_door")
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		t.Fatalf("create segment dir: %v", err)
	}

	goodPath := filepath.Join(segDir, "2026-01-01_00-00-00.mp4")
	writeProbeableSegment(t, goodPath)

	// A truncated file, which is what a segment left behind by a crash or a
	// full volume looks like. ProbeDuration cannot read a duration from it.
	badPath := filepath.Join(segDir, "2026-01-01_00-01-00.mp4")
	if err := os.WriteFile(badPath, []byte("not an mp4 at all"), 0o644); err != nil {
		t.Fatalf("write truncated segment: %v", err)
	}

	rig.sr.ScanExistingSegments("front_door", segDir)

	recs, err := rig.sr.db.GetAllSegments("front_door")
	if err != nil {
		t.Fatalf("query segments: %v", err)
	}
	byPath := make(map[string]storage.SegmentRecord, len(recs))
	for _, rec := range recs {
		byPath[rec.Path] = rec
	}

	// Positive control: without this the whole assertion below is vacuous.
	good, ok := byPath[goodPath]
	if !ok {
		t.Fatalf("readable segment %s was not imported: the scan did not run", goodPath)
	}
	if d := good.EndTime.Sub(good.StartTime); d <= 0 {
		t.Fatalf("readable segment imported with a %v duration, want > 0", d)
	}

	if bad, ok := byPath[badPath]; ok {
		t.Fatalf("segment %s whose duration could not be read was imported with a %v duration: a missing duration is recorded as a real zero-length segment",
			badPath, bad.EndTime.Sub(bad.StartTime))
	}
}
