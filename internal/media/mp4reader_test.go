package media

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
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

	playlist, err := GenerateHLSPlaylist(
		[]string{path},
		[]string{"/recordings/test.mp4"},
		0,
	)
	if err != nil {
		t.Fatalf("GenerateHLSPlaylist: %v", err)
	}

	// Verify required HLS tags are present
	requiredTags := []string{
		"#EXTM3U",
		"#EXT-X-VERSION:7",
		"#EXT-X-TARGETDURATION:",
		"#EXT-X-PLAYLIST-TYPE:VOD",
		"#EXT-X-MAP:",
		"#EXTINF:",
		"#EXT-X-BYTERANGE:",
		"#EXT-X-ENDLIST",
	}
	for _, tag := range requiredTags {
		if !strings.Contains(playlist, tag) {
			t.Errorf("playlist missing required tag %q", tag)
		}
	}

	// Verify the segment URI appears
	if !strings.Contains(playlist, "/recordings/test.mp4") {
		t.Error("playlist missing segment URI")
	}

	t.Logf("Generated playlist:\n%s", playlist)
}

func TestGenerateHLSPlaylistReal(t *testing.T) {
	const realPath = "/tmp/test_fmp4.mp4"
	if _, err := os.Stat(realPath); os.IsNotExist(err) {
		t.Skip("skipping: real test file not available at", realPath)
	}

	playlist, err := GenerateHLSPlaylist(
		[]string{realPath},
		[]string{"/recordings/real.mp4"},
		0,
	)
	if err != nil {
		t.Fatalf("GenerateHLSPlaylist: %v", err)
	}

	// A 10-minute recording should produce multiple HLS segments
	segmentCount := strings.Count(playlist, "#EXTINF:")
	if segmentCount < 2 {
		t.Errorf("expected multiple HLS segments for a long recording, got %d", segmentCount)
	}

	t.Logf("Generated %d segments from real file", segmentCount)
	t.Logf("Playlist:\n%s", playlist)
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
		if !frag.isSync {
			t.Errorf("fragment %d: expected isSync=true (IDR frame), got false", i)
		}
	}
}
