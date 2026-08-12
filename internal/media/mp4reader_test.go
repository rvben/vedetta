package media

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
)

// writeSyntheticFMP4 creates a minimal fMP4 file with synthetic H264 data.
func writeSyntheticFMP4(t *testing.T, path string, numFragments int, frameDuration uint32) {
	t.Helper()

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	defer f.Close()

	// Minimal SPS for 320x240
	sps := []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2}
	pps := []byte{0x68, 0xce, 0x38, 0x80}

	init := fmp4.Init{
		Tracks: []*fmp4.InitTrack{
			{
				ID:        1,
				TimeScale: 90000,
				Codec:     &codecs.H264{SPS: sps, PPS: pps},
			},
		},
	}
	if err := init.Marshal(f); err != nil {
		t.Fatalf("write init: %v", err)
	}

	var baseTime uint64
	for i := range numFragments {
		// Create a synthetic IDR NAL unit
		idrData := []byte{0x65, 0x88} // IDR slice
		for j := range 50 {
			idrData = append(idrData, byte(i*50+j))
		}

		avcc := h264.AVCC([][]byte{idrData})
		payload, err := avcc.Marshal()
		if err != nil {
			t.Fatalf("marshal AVCC: %v", err)
		}

		sample := &fmp4.Sample{
			Duration:        frameDuration,
			Payload:         payload,
			IsNonSyncSample: false,
		}

		part := fmp4.Part{
			SequenceNumber: uint32(i + 1),
			Tracks: []*fmp4.PartTrack{
				{
					ID:       1,
					BaseTime: baseTime,
					Samples:  []*fmp4.Sample{sample},
				},
			},
		}
		if err := part.Marshal(f); err != nil {
			t.Fatalf("write part %d: %v", i, err)
		}
		baseTime += uint64(frameDuration)
	}
}

// writeMultiTrackFMP4 creates a minimal fMP4 file with one video track (90kHz)
// and one audio track (16kHz). Each fragment contains both a video and an audio
// sample, producing a single moof with two trafs sharing one mdat — matching
// the on-disk format produced by SegmentWriter.
func writeMultiTrackFMP4(t *testing.T, path string, numFragments int, videoSampleDur, audioSampleDur uint32) {
	t.Helper()

	const videoTrackID = 1
	const audioTrackID = 2
	const videoTimeScale = 90000
	const audioTimeScale = 16000

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	defer f.Close()

	sps := []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2}
	pps := []byte{0x68, 0xce, 0x38, 0x80}

	init := fmp4.Init{
		Tracks: []*fmp4.InitTrack{
			{
				ID:        videoTrackID,
				TimeScale: videoTimeScale,
				Codec:     &codecs.H264{SPS: sps, PPS: pps},
			},
			{
				ID:        audioTrackID,
				TimeScale: audioTimeScale,
				Codec: &codecs.MPEG4Audio{
					Config: mpeg4audio.AudioSpecificConfig{
						Type:          mpeg4audio.ObjectTypeAACLC,
						SampleRate:    16000,
						ChannelConfig: 1,
					},
				},
			},
		},
	}
	if err := init.Marshal(f); err != nil {
		t.Fatalf("write init: %v", err)
	}

	var videoBaseTime, audioBaseTime uint64
	for i := range numFragments {
		idrData := []byte{0x65, 0x88}
		for j := range 50 {
			idrData = append(idrData, byte(i*50+j))
		}
		avcc := h264.AVCC([][]byte{idrData})
		videoPayload, err := avcc.Marshal()
		if err != nil {
			t.Fatalf("marshal AVCC: %v", err)
		}

		audioPayload := make([]byte, 32)
		for j := range audioPayload {
			audioPayload[j] = byte(i + j)
		}

		part := fmp4.Part{
			SequenceNumber: uint32(i + 1),
			Tracks: []*fmp4.PartTrack{
				{
					ID:       videoTrackID,
					BaseTime: videoBaseTime,
					Samples: []*fmp4.Sample{{
						Duration:        videoSampleDur,
						Payload:         videoPayload,
						IsNonSyncSample: false,
					}},
				},
				{
					ID:       audioTrackID,
					BaseTime: audioBaseTime,
					Samples: []*fmp4.Sample{{
						Duration:        audioSampleDur,
						Payload:         audioPayload,
						IsNonSyncSample: false,
					}},
				},
			},
		}
		if err := part.Marshal(f); err != nil {
			t.Fatalf("write part %d: %v", i, err)
		}
		videoBaseTime += uint64(videoSampleDur)
		audioBaseTime += uint64(audioSampleDur)
	}
}

// countTopLevelBoxes returns the count of top-level boxes of the given type.
func countTopLevelBoxes(t *testing.T, path, boxType string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	count := 0
	off := 0
	for off+8 <= len(data) {
		size := int(binary.BigEndian.Uint32(data[off : off+4]))
		typ := string(data[off+4 : off+8])
		if size == 1 && off+16 <= len(data) {
			size = int(binary.BigEndian.Uint64(data[off+8 : off+16]))
		} else if size == 0 {
			size = len(data) - off
		}
		if size < 8 || off+size > len(data) {
			break
		}
		if typ == boxType {
			count++
		}
		off += size
	}
	return count
}

func TestProbeDuration_FMP4(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.mp4")

	// 30 fragments at 3000 ticks each (90kHz) = 30 * 33.3ms = ~1 second
	writeSyntheticFMP4(t, path, 30, 3000)

	dur, err := ProbeDuration(path)
	if err != nil {
		t.Fatalf("ProbeDuration: %v", err)
	}

	expected := time.Second
	tolerance := 100 * time.Millisecond
	if dur < expected-tolerance || dur > expected+tolerance {
		t.Errorf("duration = %v, want ~%v", dur, expected)
	}
}

func TestProbeDuration_Empty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.mp4")

	// Write just the init segment with no fragments
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	sps := []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2}
	pps := []byte{0x68, 0xce, 0x38, 0x80}

	init := fmp4.Init{
		Tracks: []*fmp4.InitTrack{
			{
				ID:        1,
				TimeScale: 90000,
				Codec:     &codecs.H264{SPS: sps, PPS: pps},
			},
		},
	}
	if err := init.Marshal(f); err != nil {
		t.Fatalf("write init: %v", err)
	}
	f.Close()

	dur, err := ProbeDuration(path)
	if err != nil {
		t.Fatalf("ProbeDuration: %v", err)
	}

	if dur != 0 {
		t.Errorf("duration = %v, want 0 for empty fMP4", dur)
	}
}

func TestTrimMP4(t *testing.T) {
	dir := t.TempDir()
	inputPath := filepath.Join(dir, "input.mp4")
	outputPath := filepath.Join(dir, "output.mp4")

	// 90 fragments at 3000 ticks = 3 seconds at 90kHz
	writeSyntheticFMP4(t, inputPath, 90, 3000)

	// Trim to second 1-2
	err := TrimMP4(inputPath, outputPath, time.Second, time.Second)
	if err != nil {
		t.Fatalf("TrimMP4: %v", err)
	}

	// Verify output exists and is smaller than input
	inInfo, _ := os.Stat(inputPath)
	outInfo, err := os.Stat(outputPath)
	if err != nil {
		t.Fatalf("output not created: %v", err)
	}
	if outInfo.Size() >= inInfo.Size() {
		t.Errorf("trimmed file (%d bytes) should be smaller than input (%d bytes)",
			outInfo.Size(), inInfo.Size())
	}
	if outInfo.Size() == 0 {
		t.Error("trimmed file is empty")
	}

	// Verify the trimmed file has valid duration
	dur, err := ProbeDuration(outputPath)
	if err != nil {
		t.Fatalf("ProbeDuration on trimmed: %v", err)
	}
	// Should be approximately 1 second
	if dur < 800*time.Millisecond || dur > 1200*time.Millisecond {
		t.Errorf("trimmed duration = %v, want ~1s", dur)
	}
}

// TestTrimMP4_MultiTrackNoMoofDuplication asserts that trimming a multi-track
// fMP4 (video + audio per moof) writes each source moof exactly once. A regression
// caused indexFile to emit one fragment per traf, which made TrimMP4 copy the
// shared moof+mdat once per traf — duplicating moofs in the output and inflating
// the player-reported duration.
func TestTrimMP4_MultiTrackNoMoofDuplication(t *testing.T) {
	dir := t.TempDir()
	inputPath := filepath.Join(dir, "input.mp4")
	outputPath := filepath.Join(dir, "output.mp4")

	// 30 fragments of 100ms each: video=9000@90k, audio=1600@16k. Total 3s.
	writeMultiTrackFMP4(t, inputPath, 30, 9000, 1600)

	// Trim a 1-second window starting at 1s. Should include ~10 fragments.
	if err := TrimMP4(inputPath, outputPath, time.Second, time.Second); err != nil {
		t.Fatalf("TrimMP4: %v", err)
	}

	moofs := countTopLevelBoxes(t, outputPath, "moof")
	mdats := countTopLevelBoxes(t, outputPath, "mdat")
	if moofs != mdats {
		t.Errorf("moof count (%d) must equal mdat count (%d) — moofs are being duplicated", moofs, mdats)
	}

	dur, err := ProbeDuration(outputPath)
	if err != nil {
		t.Fatalf("ProbeDuration: %v", err)
	}
	if dur < 800*time.Millisecond || dur > 1200*time.Millisecond {
		t.Errorf("trimmed duration = %v, want ~1s (duplicated moofs would report ~2s)", dur)
	}
}

// TestConcatMP4_MultiTrackNoMoofDuplication asserts the same invariant for
// ConcatMP4: one moof per source moof in the output.
func TestConcatMP4_MultiTrackNoMoofDuplication(t *testing.T) {
	dir := t.TempDir()
	in1 := filepath.Join(dir, "seg1.mp4")
	in2 := filepath.Join(dir, "seg2.mp4")
	outputPath := filepath.Join(dir, "output.mp4")

	writeMultiTrackFMP4(t, in1, 10, 9000, 1600)
	writeMultiTrackFMP4(t, in2, 10, 9000, 1600)

	if err := ConcatMP4([]string{in1, in2}, outputPath, 0, 0); err != nil {
		t.Fatalf("ConcatMP4: %v", err)
	}

	moofs := countTopLevelBoxes(t, outputPath, "moof")
	mdats := countTopLevelBoxes(t, outputPath, "mdat")
	if moofs != mdats {
		t.Errorf("moof count (%d) must equal mdat count (%d) — moofs are being duplicated", moofs, mdats)
	}
	if moofs != 20 {
		t.Errorf("expected 20 moofs (10 per input), got %d", moofs)
	}

	dur, err := ProbeDuration(outputPath)
	if err != nil {
		t.Fatalf("ProbeDuration: %v", err)
	}
	if dur < 1800*time.Millisecond || dur > 2200*time.Millisecond {
		t.Errorf("concatenated duration = %v, want ~2s", dur)
	}
}

func TestTrimMP4_FullRange(t *testing.T) {
	dir := t.TempDir()
	inputPath := filepath.Join(dir, "input.mp4")
	outputPath := filepath.Join(dir, "output.mp4")

	writeSyntheticFMP4(t, inputPath, 30, 3000)

	// Trim with full range should keep everything
	err := TrimMP4(inputPath, outputPath, 0, 10*time.Second)
	if err != nil {
		t.Fatalf("TrimMP4: %v", err)
	}

	inInfo, _ := os.Stat(inputPath)
	outInfo, _ := os.Stat(outputPath)

	// Should be the same size (same fragments)
	if outInfo.Size() != inInfo.Size() {
		t.Errorf("full-range trim: output %d bytes, input %d bytes", outInfo.Size(), inInfo.Size())
	}
}

func TestConcatMP4(t *testing.T) {
	dir := t.TempDir()
	path1 := filepath.Join(dir, "seg1.mp4")
	path2 := filepath.Join(dir, "seg2.mp4")
	outputPath := filepath.Join(dir, "concat.mp4")

	// Two 1-second segments
	writeSyntheticFMP4(t, path1, 30, 3000)
	writeSyntheticFMP4(t, path2, 30, 3000)

	err := ConcatMP4([]string{path1, path2}, outputPath, 0, 0)
	if err != nil {
		t.Fatalf("ConcatMP4: %v", err)
	}

	outInfo, err := os.Stat(outputPath)
	if err != nil {
		t.Fatalf("output not created: %v", err)
	}
	if outInfo.Size() == 0 {
		t.Error("concatenated file is empty")
	}

	// Concatenated should have ~2 seconds duration
	dur, err := ProbeDuration(outputPath)
	if err != nil {
		t.Fatalf("ProbeDuration on concat: %v", err)
	}
	if dur < 1800*time.Millisecond || dur > 2200*time.Millisecond {
		t.Errorf("concat duration = %v, want ~2s", dur)
	}
}

func TestConcatMP4_SingleFile(t *testing.T) {
	dir := t.TempDir()
	inputPath := filepath.Join(dir, "input.mp4")
	outputPath := filepath.Join(dir, "output.mp4")

	writeSyntheticFMP4(t, inputPath, 30, 3000)

	err := ConcatMP4([]string{inputPath}, outputPath, 0, 0)
	if err != nil {
		t.Fatalf("ConcatMP4: %v", err)
	}

	dur, err := ProbeDuration(outputPath)
	if err != nil {
		t.Fatalf("ProbeDuration: %v", err)
	}
	if dur < 900*time.Millisecond || dur > 1100*time.Millisecond {
		t.Errorf("duration = %v, want ~1s", dur)
	}
}

func TestConcatMP4_Empty(t *testing.T) {
	dir := t.TempDir()
	outputPath := filepath.Join(dir, "output.mp4")

	err := ConcatMP4(nil, outputPath, 0, 0)
	if err == nil {
		t.Fatal("expected error for empty inputs")
	}
}

func TestGenerateHLSPlaylist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.mp4")

	// 10 fragments at 3000 ticks each (90kHz timescale) = ~333ms total
	writeSyntheticFMP4(t, path, 10, 3000)

	result, err := GenerateHLSPlaylist(
		[]string{path},
		[]string{"/recordings/test.mp4"},
		nil,
		0,
	)
	if err != nil {
		t.Fatalf("GenerateHLSPlaylist: %v", err)
	}

	playlist := result.Playlist

	// Verify required HLS tags are present
	requiredTags := []string{
		"#EXTM3U",
		"#EXT-X-VERSION:7",
		"#EXT-X-TARGETDURATION:",
		"#EXT-X-PLAYLIST-TYPE:VOD",
		"#EXT-X-MAP:",
		"#EXTINF:",
		"#EXT-X-ENDLIST",
	}
	for _, tag := range requiredTags {
		if !strings.Contains(playlist, tag) {
			t.Errorf("playlist missing required tag %q", tag)
		}
	}

	// Verify segment refs are populated
	if len(result.Segments) == 0 {
		t.Error("no segment refs produced")
	}

	t.Logf("Generated playlist:\n%s", playlist)
}

// Multi-file playlists must signal the decode-time reset at every file
// boundary (EXT-X-DISCONTINUITY) and anchor each file to wall-clock time
// (EXT-X-PROGRAM-DATE-TIME), with the first file's PDT advanced to the retained
// GOP and EXT-X-START carrying the exact sub-GOP offset.
func TestGenerateHLSPlaylist_DiscontinuityAndPDT(t *testing.T) {
	dir := t.TempDir()
	path1 := filepath.Join(dir, "a.mp4")
	path2 := filepath.Join(dir, "b.mp4")

	// 10 fragments of 1s each per file (every fragment is a keyframe).
	writeSyntheticFMP4(t, path1, 10, 90000)
	writeSyntheticFMP4(t, path2, 10, 90000)

	t0 := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	fileStarts := []time.Time{t0, t0.Add(20 * time.Second)}

	// Request 5.25s into the first file. The retained GOP starts at 5s, and the
	// player must then advance another 250ms for frame-accurate presentation.
	result, err := GenerateHLSPlaylist(
		[]string{path1, path2},
		[]string{"/a", "/b"},
		fileStarts,
		5250*time.Millisecond,
	)
	if err != nil {
		t.Fatalf("GenerateHLSPlaylist: %v", err)
	}
	playlist := result.Playlist

	if got := strings.Count(playlist, "#EXT-X-DISCONTINUITY\n"); got != 1 {
		t.Errorf("got %d discontinuity tags, want 1 (one file boundary):\n%s", got, playlist)
	}
	// First file's PDT reflects the 5s trim.
	if !strings.Contains(playlist, "#EXT-X-PROGRAM-DATE-TIME:2026-06-10T12:00:05.000Z") {
		t.Errorf("missing trimmed first-file PDT:\n%s", playlist)
	}
	if !strings.Contains(playlist, "#EXT-X-START:TIME-OFFSET=0.250000,PRECISE=YES") {
		t.Errorf("missing precise sub-GOP start offset:\n%s", playlist)
	}
	// Second file's PDT is its own wall start (media restarts at tick 0).
	if !strings.Contains(playlist, "#EXT-X-PROGRAM-DATE-TIME:2026-06-10T12:00:20.000Z") {
		t.Errorf("missing second-file PDT:\n%s", playlist)
	}
	// The discontinuity must precede the second file's init map.
	disc := strings.Index(playlist, "#EXT-X-DISCONTINUITY")
	secondMap := strings.Index(playlist, "#EXT-X-MAP:URI=\"/b/hls/init.mp4\"")
	if secondMap == -1 || disc == -1 || disc > secondMap {
		t.Errorf("discontinuity not before second file's map (disc=%d, map=%d):\n%s", disc, secondMap, playlist)
	}
}

// Without wall-clock starts the playlist must omit PDT tags entirely rather
// than emit a bogus epoch.
func TestGenerateHLSPlaylist_NoPDTWithoutFileStarts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.mp4")
	writeSyntheticFMP4(t, path, 5, 90000)

	result, err := GenerateHLSPlaylist([]string{path}, []string{"/a"}, nil, 0)
	if err != nil {
		t.Fatalf("GenerateHLSPlaylist: %v", err)
	}
	if strings.Contains(result.Playlist, "PROGRAM-DATE-TIME") {
		t.Errorf("unexpected PDT without fileStarts:\n%s", result.Playlist)
	}
}

func TestGenerateHLSPlaylistReal(t *testing.T) {
	const realPath = "/tmp/test_fmp4.mp4"
	if _, err := os.Stat(realPath); os.IsNotExist(err) {
		t.Skip("skipping: real test file not available at", realPath)
	}

	result, err := GenerateHLSPlaylist(
		[]string{realPath},
		[]string{"/recordings/real.mp4"},
		nil,
		0,
	)
	if err != nil {
		t.Fatalf("GenerateHLSPlaylist: %v", err)
	}

	// A 10-minute recording should produce multiple HLS segments
	segmentCount := strings.Count(result.Playlist, "#EXTINF:")
	if segmentCount < 2 {
		t.Errorf("expected multiple HLS segments for a long recording, got %d", segmentCount)
	}

	t.Logf("Generated %d segments from real file", segmentCount)
	t.Logf("Playlist:\n%s", result.Playlist)
}

func TestIndexFileDetectsSync(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.mp4")

	// 10 fragments, each with one sync (IDR) sample
	writeSyntheticFMP4(t, path, 10, 3000)

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	_, fragments, _, err := indexFile(f)
	if err != nil {
		t.Fatalf("indexFile: %v", err)
	}

	if len(fragments) != 10 {
		t.Fatalf("expected 10 fragments, got %d", len(fragments))
	}

	for i, frag := range fragments {
		if len(frag.trafs) == 0 || !frag.trafs[0].isSync {
			t.Errorf("fragment %d: expected isSync=true (IDR frame), got false", i)
		}
	}
}

func TestServeHLSSegment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.mp4")
	writeSyntheticFMP4(t, path, 10, 3000)

	result, err := GenerateHLSPlaylist([]string{path}, []string{"/test"}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Segments) == 0 {
		t.Fatal("no segments")
	}

	// Serve the first segment
	ref := result.Segments[0]
	var buf bytes.Buffer
	err = ServeHLSSegment(&buf, ref.FilePath, ref.ByteStart, ref.ByteEnd)
	if err != nil {
		t.Fatalf("ServeHLSSegment: %v", err)
	}

	if buf.Len() == 0 {
		t.Fatal("empty output")
	}

	// Verify the output starts with a moof box
	if buf.Len() < 8 {
		t.Fatal("output too small")
	}
	boxType := string(buf.Bytes()[4:8])
	if boxType != "moof" {
		t.Errorf("expected moof box, got %q", boxType)
	}

	t.Logf("Re-segmented %d bytes from range [%d, %d)", buf.Len(), ref.ByteStart, ref.ByteEnd)
}
