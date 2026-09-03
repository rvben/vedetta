package media

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
)

// fixtureWithoutFragments writes a copy of the fixture truncated just before its
// first moof: a valid header declaring an H264 track, with no fragments behind
// it. That is the state a progressive (non-fragmented) MP4 reaches in this
// reader - the container parses and yields zero moof blocks - and one of the
// segments that failed in production was exactly that shape.
func fixtureWithoutFragments(t *testing.T) string {
	t.Helper()

	fixture := filepath.Join("testdata", "sample_segment.mp4")
	src, err := openTranscodeSource(fixture)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer src.file.Close()
	if len(src.blocks) == 0 {
		t.Fatal("fixture has no moof blocks, so it cannot be truncated to none")
	}

	if _, err := src.file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	head := make([]byte, src.blocks[0].moofOffset)
	if _, err := io.ReadFull(src.file, head); err != nil {
		t.Fatalf("read init segment: %v", err)
	}

	path := filepath.Join(t.TempDir(), "no_fragments.mp4")
	if err := os.WriteFile(path, head, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fixtureWithUndecodableVideo writes a copy of the fixture whose video slice
// payloads are scrambled while every NAL header byte, every sample boundary and
// the whole container are preserved. The decoder therefore receives a genuine
// IDR NAL and rejects its contents, which is what 16 of the 18 segments that
// failed in production did.
func fixtureWithUndecodableVideo(t *testing.T) string {
	t.Helper()

	fixture := filepath.Join("testdata", "sample_segment.mp4")
	src, err := openTranscodeSource(fixture)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer src.file.Close()

	path := filepath.Join(t.TempDir(), "undecodable.mp4")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()

	if err := src.init.Marshal(out); err != nil {
		t.Fatalf("write init: %v", err)
	}

	var seqNum uint32 = 1
	for i, blk := range src.blocks {
		parts, err := readGOPBlock(src.file, blk, i)
		if err != nil {
			t.Fatalf("read block %d: %v", i, err)
		}
		var tracks []*fmp4.PartTrack
		for _, part := range parts {
			for _, tr := range part.Tracks {
				samples := make([]*fmp4.Sample, 0, len(tr.Samples))
				for _, s := range tr.Samples {
					payload := append([]byte(nil), s.Payload...)
					if tr.ID == src.videoTrackID {
						scrambleNALBodies(payload)
					}
					samples = append(samples, &fmp4.Sample{
						Duration:        s.Duration,
						PTSOffset:       s.PTSOffset,
						IsNonSyncSample: s.IsNonSyncSample,
						Payload:         payload,
					})
				}
				if len(samples) == 0 {
					continue
				}
				tracks = append(tracks, &fmp4.PartTrack{
					ID:       tr.ID,
					BaseTime: tr.BaseTime,
					Samples:  samples,
				})
			}
		}
		if len(tracks) == 0 {
			continue
		}
		outPart := fmp4.Part{SequenceNumber: seqNum, Tracks: tracks}
		if err := outPart.Marshal(out); err != nil {
			t.Fatalf("write part %d: %v", seqNum, err)
		}
		seqNum++
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// scrambleNALBodies walks AVCC length-prefixed NALs in place, keeping every
// length field and every NAL header byte so the container and the NAL types
// stay readable, and destroying the slice data behind them.
func scrambleNALBodies(avcc []byte) {
	for off := 0; off+4 <= len(avcc); {
		nalLen := int(binary.BigEndian.Uint32(avcc[off:]))
		off += 4
		if nalLen <= 0 || off+nalLen > len(avcc) {
			return
		}
		for i := off + 1; i < off+nalLen; i++ {
			avcc[i] ^= 0x5a
		}
		off += nalLen
	}
}

// TestTranscodeNamesTheContainerWhenThereAreNoFragments pins the first of the
// two shapes that reported "no frames encoded successfully" in production. A
// file with no fragments has nothing to recompress and never will have; saying
// so is what lets the caller retire it instead of retrying it forever.
func TestTranscodeNamesTheContainerWhenThereAreNoFragments(t *testing.T) {
	ensureOpenH264ForTest(t)

	dst := filepath.Join(t.TempDir(), "out.mp4")
	err := transcodeFile(fixtureWithoutFragments(t), dst, 1280, 720)
	if err == nil {
		t.Fatal("transcodeFile accepted a source with no fragments")
	}

	var te *TranscodeError
	if !errors.As(err, &te) {
		t.Fatalf("error is %T (%v), want a *TranscodeError naming the cause", err, err)
	}
	if te.Kind != TranscodeSourceNotFragmented {
		t.Errorf("kind is %q, want %q", te.Kind, TranscodeSourceNotFragmented)
	}
	if te.Retryable() {
		t.Error("a file with no fragments is reported as retryable, but no retry can add fragments to it")
	}
}

// TestTranscodeNamesTheSourceWhenNoFrameDecodes pins the second shape, and the
// larger one: the container is fine and the camera's video inside it is not
// decodable. That is not a recompression fault, and the error has to say so or
// the operator reads it as one.
func TestTranscodeNamesTheSourceWhenNoFrameDecodes(t *testing.T) {
	ensureOpenH264ForTest(t)

	dst := filepath.Join(t.TempDir(), "out.mp4")
	err := transcodeFile(fixtureWithUndecodableVideo(t), dst, 1280, 720)
	if err == nil {
		t.Fatal("transcodeFile accepted a source whose every frame is undecodable")
	}

	var te *TranscodeError
	if !errors.As(err, &te) {
		t.Fatalf("error is %T (%v), want a *TranscodeError naming the cause", err, err)
	}
	if te.Kind != TranscodeSourceUndecodable {
		t.Errorf("kind is %q, want %q", te.Kind, TranscodeSourceUndecodable)
	}
	if te.Retryable() {
		t.Error("undecodable source video is reported as retryable, but the bytes on disk do not change")
	}
}

// TestUndecodableFixtureIsOtherwiseIntact is the negative control for the
// fixture above: it must fail for the reason the test claims. If the scrambling
// broke the container instead of the video, the classification test would pass
// while proving nothing about undecodable video.
func TestUndecodableFixtureIsOtherwiseIntact(t *testing.T) {
	src, err := openTranscodeSource(fixtureWithUndecodableVideo(t))
	if err != nil {
		t.Fatalf("scrambled fixture no longer parses as fMP4: %v", err)
	}
	defer src.file.Close()

	if len(src.blocks) == 0 {
		t.Fatal("scrambled fixture has no fragments, so it tests the wrong failure")
	}

	idrBlocks := 0
	for i, blk := range src.blocks {
		parts, err := readGOPBlock(src.file, blk, i)
		if err != nil {
			t.Fatalf("read block %d: %v", i, err)
		}
		gop := splitGOPTracks(parts, src.videoTrackID, src.audioTrackID)
		annexB, err := avccToAnnexB(gop.videoAVCC)
		if err != nil {
			t.Fatalf("block %d no longer holds well-formed AVCC: %v", i, err)
		}
		for _, nal := range splitAnnexB(annexB) {
			if len(nal) > 0 && nal[0]&0x1f == 5 {
				idrBlocks++
				break
			}
		}
	}
	if idrBlocks == 0 {
		t.Error("scrambled fixture has no IDR NALs left; the decoder would never see a keyframe to reject")
	}
}
