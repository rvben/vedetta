package media

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
)

// writeGOPFMP4 writes an fMP4 whose fragments open on a sync sample only at the
// given indices. SegmentWriter normally starts every fragment on a keyframe,
// but capPendingPart force-flushes a partial GOP when no keyframe arrives in
// time, so a recording can legitimately contain fragments that open on a P
// frame. Each fragment is one sample of frameDuration ticks.
func writeGOPFMP4(t *testing.T, path string, numFragments int, frameDuration uint32, syncAt map[int]bool) {
	t.Helper()

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	defer f.Close()

	sps := []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2}
	pps := []byte{0x68, 0xce, 0x38, 0x80}

	init := fmp4.Init{
		Tracks: []*fmp4.InitTrack{
			{ID: 1, TimeScale: 90000, Codec: &codecs.H264{SPS: sps, PPS: pps}},
		},
	}
	if err := init.Marshal(f); err != nil {
		t.Fatalf("write init: %v", err)
	}

	var baseTime uint64
	for i := range numFragments {
		nalHeader := byte(0x41) // non-IDR slice
		if syncAt[i] {
			nalHeader = 0x65 // IDR slice
		}
		nal := []byte{nalHeader, 0x88}
		for j := range 40 {
			nal = append(nal, byte(i*40+j))
		}
		payload, err := h264.AVCC([][]byte{nal}).Marshal()
		if err != nil {
			t.Fatalf("marshal AVCC: %v", err)
		}

		part := fmp4.Part{
			SequenceNumber: uint32(i + 1),
			Tracks: []*fmp4.PartTrack{{
				ID:       1,
				BaseTime: baseTime,
				Samples: []*fmp4.Sample{{
					Duration:        frameDuration,
					Payload:         payload,
					IsNonSyncSample: !syncAt[i],
				}},
			}},
		}
		if err := part.Marshal(f); err != nil {
			t.Fatalf("write part %d: %v", i, err)
		}
		baseTime += uint64(frameDuration)
	}
}

// firstFragmentIsSync reports whether the first fragment of an fMP4 opens on a
// sync sample, which is what a decoder needs to render the clip's first frame.
func firstFragmentIsSync(t *testing.T, path string) bool {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	_, fragments, timeScales, err := indexFile(f)
	if err != nil {
		t.Fatalf("index %s: %v", path, err)
	}
	if len(fragments) == 0 {
		t.Fatalf("%s has no fragments, so the assertion would pass vacuously", path)
	}
	videoTrackID := findVideoTrack(fragments, timeScales)
	traf := fragments[0].traf(videoTrackID)
	if traf == nil {
		t.Fatalf("%s: first fragment has no video traf", path)
	}
	return traf.isSync
}

// A clip must open on a frame a decoder can start from. Selecting fragments by
// window overlap alone begins wherever the window happens to land, so a request
// that starts inside a GOP produces a clip whose first frames reference a
// keyframe that is not in the file: the event thumbnail and the opening second,
// the part of the clip anyone actually looks at, are unrenderable.
func TestTrimMP4_StartsOnAKeyframe(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.mp4")
	out := filepath.Join(dir, "out.mp4")

	// One-second fragments, keyframes at 0s and 4s.
	const frameDuration = 90000
	writeGOPFMP4(t, in, 8, frameDuration, map[int]bool{0: true, 4: true})
	if !firstFragmentIsSync(t, in) {
		t.Fatal("fixture is wrong: the source does not start on a sync sample")
	}

	// The window opens two seconds in, inside the first GOP.
	if err := TrimMP4(in, out, 2*time.Second, 2*time.Second); err != nil {
		t.Fatalf("TrimMP4: %v", err)
	}
	if !firstFragmentIsSync(t, out) {
		t.Error("the trimmed clip opens on a non-sync sample, so its first frames cannot be decoded")
	}
}

// The same requirement for the streaming form, which serves an in-progress
// recording to a player that has no earlier fragment to fall back on.
func TestTrimMP4ToWriter_StartsOnAKeyframe(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.mp4")
	out := filepath.Join(dir, "out.mp4")

	const frameDuration = 90000
	writeGOPFMP4(t, in, 8, frameDuration, map[int]bool{0: true, 4: true})

	f, err := os.Create(out)
	if err != nil {
		t.Fatalf("create out: %v", err)
	}
	if err := TrimMP4ToWriter(in, f, 6*time.Second); err != nil {
		f.Close()
		t.Fatalf("TrimMP4ToWriter: %v", err)
	}
	f.Close()

	if !firstFragmentIsSync(t, out) {
		t.Error("the streamed clip opens on a non-sync sample, so its first frames cannot be decoded")
	}
}

// Backing up to a keyframe must not throw away a clip that already starts on
// one: an aligned request has to copy exactly the fragments it asked for.
func TestTrimMP4_AlignedStartKeepsTheRequestedWindow(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.mp4")
	out := filepath.Join(dir, "out.mp4")

	const frameDuration = 90000
	writeGOPFMP4(t, in, 8, frameDuration, map[int]bool{0: true, 4: true})

	if err := TrimMP4(in, out, 4*time.Second, 2*time.Second); err != nil {
		t.Fatalf("TrimMP4: %v", err)
	}

	f, err := os.Open(out)
	if err != nil {
		t.Fatalf("open out: %v", err)
	}
	defer f.Close()
	_, fragments, _, err := indexFile(f)
	if err != nil {
		t.Fatalf("index out: %v", err)
	}
	if len(fragments) != 2 {
		t.Errorf("trimmed clip has %d fragments, want 2: the request already started on a keyframe, so nothing should be prepended", len(fragments))
	}
}

// A recording with no keyframe at all before the requested window is
// undecodable wherever the clip starts, so backing up further buys nothing and
// costs the whole file. The lower bound must stay at the overlapping fragment.
//
// This is not a hypothetical shape for the reader to encounter: it is what a
// stream that stopped emitting keyframes produces once capPendingPart has
// force-flushed partial GOPs for the whole segment. SegmentWriter will not open
// a file on a non-keyframe, so a fragment 0 that is not a sync sample means the
// file was written by something else or was truncated ahead of its first GOP.
func TestTrimMP4_NoKeyframeAnywhereKeepsTheRequestedWindow(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.mp4")
	out := filepath.Join(dir, "out.mp4")

	const frameDuration = 90000
	// No entry is a sync sample, so there is nothing to back up to.
	writeGOPFMP4(t, in, 8, frameDuration, map[int]bool{})
	if firstFragmentIsSync(t, in) {
		t.Fatal("fixture is wrong: this test needs a source with no keyframe at all")
	}

	if err := TrimMP4(in, out, 6*time.Second, 2*time.Second); err != nil {
		t.Fatalf("TrimMP4: %v", err)
	}

	f, err := os.Open(out)
	if err != nil {
		t.Fatalf("open out: %v", err)
	}
	defer f.Close()
	_, fragments, _, err := indexFile(f)
	if err != nil {
		t.Fatalf("index out: %v", err)
	}
	// Fragments 6 and 7 cover [6s, 8s). Falling back to index 0 would copy all
	// eight, prepending six seconds of equally undecodable video.
	if len(fragments) != 2 {
		t.Errorf("trimmed clip has %d fragments, want 2: with no keyframe to back up to, the window itself is the lower bound", len(fragments))
	}
}
