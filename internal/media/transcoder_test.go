package media

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
)

var (
	mediaTestBinaryOnce sync.Once
	mediaTestBinaryDir  string
	mediaTestBinaryPath string
	mediaTestBinaryErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if mediaTestBinaryDir != "" {
		_ = os.RemoveAll(mediaTestBinaryDir)
	}
	os.Exit(code)
}

func isolatedMediaTestBinary() (string, error) {
	mediaTestBinaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "vedetta-media-test-bin")
		if err != nil {
			mediaTestBinaryErr = err
			return
		}
		mediaTestBinaryDir = dir
		mediaTestBinaryPath = filepath.Join(dir, "vedetta")
		if output, err := exec.Command("go", "build", "-o", mediaTestBinaryPath, "github.com/rvben/vedetta/cmd/vedetta").CombinedOutput(); err != nil {
			mediaTestBinaryErr = fmt.Errorf("build isolated transcode worker: %w (%s)", err, output)
		}
	})
	return mediaTestBinaryPath, mediaTestBinaryErr
}

// isolatedTranscodeSegment exercises the production worker command in a normal
// (non-race-instrumented) binary. OpenH264 can corrupt memory in ways that
// surface after the call returns; the disposable process and deadline mirror
// production while the race-instrumented parent retains control of the suite.
func isolatedTranscodeSegment(t *testing.T, source string, width, height int) (TranscodeResult, error) {
	t.Helper()
	binary, err := isolatedMediaTestBinary()
	if err != nil {
		return TranscodeResult{}, err
	}
	dir := t.TempDir()
	stagePath := filepath.Join(dir, "stage.mp4")
	logPath := filepath.Join(dir, "worker.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return TranscodeResult{}, fmt.Errorf("create transcode test log: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "transcode",
		"-w", strconv.Itoa(width), "-h", strconv.Itoa(height),
		"-output", stagePath, source)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	runErr := cmd.Run()
	closeErr := logFile.Close()
	if ctx.Err() == context.DeadlineExceeded {
		return TranscodeResult{}, fmt.Errorf("isolated transcode worker timed out after 5m")
	}
	if runErr != nil {
		return TranscodeResult{}, fmt.Errorf("isolated transcode worker failed: %w (output: %s)", runErr, readTestLogTail(logPath, 8192))
	}
	if closeErr != nil {
		return TranscodeResult{}, fmt.Errorf("close transcode test log: %w", closeErr)
	}
	var result TranscodeResult
	found := false
	for _, line := range strings.Split(readTestLogTail(logPath, 64<<10), "\n") {
		payload, ok := strings.CutPrefix(line, TranscodeResultMarker)
		if !ok {
			continue
		}
		if err := json.Unmarshal([]byte(payload), &result); err != nil {
			return TranscodeResult{}, fmt.Errorf("decode isolated transcode result: %w", err)
		}
		found = true
		break
	}
	if !found {
		return TranscodeResult{}, fmt.Errorf("isolated transcode worker produced no result marker")
	}
	if !result.Skipped {
		if err := os.Rename(stagePath, source); err != nil {
			return TranscodeResult{}, fmt.Errorf("commit isolated transcode test output: %w", err)
		}
	}
	return result, nil
}

func readTestLogTail(path string, limit int64) string {
	f, err := os.Open(path)
	if err != nil {
		return err.Error()
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err.Error()
	}
	if info.Size() > limit {
		if _, err := f.Seek(info.Size()-limit, io.SeekStart); err != nil {
			return err.Error()
		}
	}
	payload, err := io.ReadAll(f)
	if err != nil {
		return err.Error()
	}
	return string(payload)
}

func TestScaleYCbCr_PreservesAspectRatio(t *testing.T) {
	// 1920x800 source, target 1280x720 → should output 1280x532
	// scale = min(1280/1920, 720/800) = min(0.666, 0.9) = 0.666
	// out_width  = floor(1920 * 0.6666 / 2) * 2 = floor(639.99) * 2 = 639*2 = 1278 → hmm
	// Actually: scale = 1280/1920 = 0.6666...
	// out_width  = floor(1920 * 0.6666 / 2) * 2 = floor(639.99) * 2 = 639*2 = 1278
	// Wait, let me recalculate per spec:
	// scale = min(1280/1920, 720/800) = min(0.6666, 0.9) = 0.6666
	// out_width = floor(1920 * 0.6666 / 2) * 2
	// 1920 * 0.6666... = 1280 exactly → floor(1280/2)*2 = 640*2 = 1280
	// out_height = floor(800 * 0.6666 / 2) * 2
	// 800 * 0.6666... = 533.33 → floor(533.33/2)*2 = floor(266.66)*2 = 266*2 = 532
	src := image.NewYCbCr(image.Rect(0, 0, 1920, 800), image.YCbCrSubsampleRatio420)
	got := scaleYCbCr(src, 1280, 720)
	if got.Rect.Dx() != 1280 {
		t.Errorf("width = %d, want 1280", got.Rect.Dx())
	}
	if got.Rect.Dy() != 532 {
		t.Errorf("height = %d, want 532", got.Rect.Dy())
	}
}

func TestScaleYCbCr_EvenDimensions(t *testing.T) {
	// Output must always have even width and height (H264 requirement)
	src := image.NewYCbCr(image.Rect(0, 0, 1921, 1081), image.YCbCrSubsampleRatio420)
	got := scaleYCbCr(src, 1280, 720)
	if got.Rect.Dx()%2 != 0 {
		t.Errorf("width %d is not even", got.Rect.Dx())
	}
	if got.Rect.Dy()%2 != 0 {
		t.Errorf("height %d is not even", got.Rect.Dy())
	}
}

// The transcoder hands scaled.Y/Cb/Cr straight to the C OpenH264 encoder via
// raw unsafe.Pointer; the C library then reads IPicHeight rows of IStride
// bytes out of those Go-owned slices across the cgo boundary. If a degenerate
// or corrupt decoded frame yields planes shorter than that traversal, C reads
// past Go heap memory - an out-of-bounds the Go bounds checker cannot see,
// exactly the wild-read signature behind the recompression crash loop.
// encoderInputValid must accept a well-formed scaleYCbCr result and reject
// anything the encoder would over-read.
func TestEncoderInputValid_AcceptsScaledOutput(t *testing.T) {
	src := image.NewYCbCr(image.Rect(0, 0, 1920, 1080), image.YCbCrSubsampleRatio420)
	scaled := scaleYCbCr(src, 1280, 720)
	if !encoderInputValid(scaled, scaled.Rect.Dx(), scaled.Rect.Dy()) {
		t.Fatalf("well-formed %dx%d scaled frame rejected", scaled.Rect.Dx(), scaled.Rect.Dy())
	}
}

func TestEncoderInputValid_RejectsBadGeometry(t *testing.T) {
	good := image.NewYCbCr(image.Rect(0, 0, 64, 64), image.YCbCrSubsampleRatio420)

	cases := []struct {
		name       string
		img        *image.YCbCr
		outW, outH int
	}{
		{"zero width", good, 0, 64},
		{"zero height", good, 64, 0},
		{"negative width", good, -64, 64},
		{"odd width", good, 63, 64},
		{"odd height", good, 64, 63},
		{
			name: "luma plane shorter than stride*height",
			img: &image.YCbCr{
				Y: make([]byte, 64*63), Cb: make([]byte, 32*32), Cr: make([]byte, 32*32),
				YStride: 64, CStride: 32, SubsampleRatio: image.YCbCrSubsampleRatio420,
				Rect: image.Rect(0, 0, 64, 64),
			},
			outW: 64, outH: 64,
		},
		{
			name: "chroma plane truncated",
			img: &image.YCbCr{
				Y: make([]byte, 64*64), Cb: make([]byte, 32*31), Cr: make([]byte, 32*32),
				YStride: 64, CStride: 32, SubsampleRatio: image.YCbCrSubsampleRatio420,
				Rect: image.Rect(0, 0, 64, 64),
			},
			outW: 64, outH: 64,
		},
		{
			name: "luma stride narrower than width",
			img: &image.YCbCr{
				Y: make([]byte, 64*64), Cb: make([]byte, 32*32), Cr: make([]byte, 32*32),
				YStride: 32, CStride: 32, SubsampleRatio: image.YCbCrSubsampleRatio420,
				Rect: image.Rect(0, 0, 64, 64),
			},
			outW: 64, outH: 64,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if encoderInputValid(c.img, c.outW, c.outH) {
				t.Fatalf("%s: expected rejection, got accepted", c.name)
			}
		})
	}
}

func TestScaleYCbCr_SameSize(t *testing.T) {
	src := image.NewYCbCr(image.Rect(0, 0, 1280, 720), image.YCbCrSubsampleRatio420)
	got := scaleYCbCr(src, 1280, 720)
	if got.Rect.Dx() != 1280 || got.Rect.Dy() != 720 {
		t.Errorf("got %dx%d, want 1280x720", got.Rect.Dx(), got.Rect.Dy())
	}
}

func TestShouldTranscode_SkipsIfAlreadySmall(t *testing.T) {
	// 720p source, 720p target → skip
	skip, outW, outH := shouldTranscode(1280, 720, 1280, 720)
	if !skip {
		t.Error("expected skip=true for same-size source")
	}
	_ = outW
	_ = outH
}

func TestShouldTranscode_SkipsIfBelowTarget(t *testing.T) {
	// 640x480 source, 1280x720 target → source smaller, skip
	skip, _, _ := shouldTranscode(640, 480, 1280, 720)
	if !skip {
		t.Error("expected skip=true when source is smaller than target")
	}
}

func TestShouldTranscode_SkipsBelowAreaThreshold(t *testing.T) {
	// 1280x800 source, 1280x720 target → area reduction ~10%, skip
	skip, _, _ := shouldTranscode(1280, 800, 1280, 720)
	if !skip {
		t.Error("expected skip=true when area reduction < 25%")
	}
}

func TestShouldTranscode_Transcodes1080p(t *testing.T) {
	// 1920x1080 source, 1280x720 target → 56% reduction, transcode
	skip, outW, outH := shouldTranscode(1920, 1080, 1280, 720)
	if skip {
		t.Error("expected skip=false for 1080p → 720p")
	}
	if outW != 1280 || outH != 720 {
		t.Errorf("got %dx%d, want 1280x720", outW, outH)
	}
}

func TestShouldTranscode_Preserves4K(t *testing.T) {
	// 2560x1440 source, 1280x720 target → 75% reduction, transcode
	skip, outW, outH := shouldTranscode(2560, 1440, 1280, 720)
	if skip {
		t.Error("expected skip=false for 4K → 720p")
	}
	if outW != 1280 || outH != 720 {
		t.Errorf("got %dx%d, want 1280x720", outW, outH)
	}
}

func TestReadSourceResolution(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/test.mp4"

	// writeSyntheticFMP4 uses SPS bytes that decode to 128x96
	writeSyntheticFMP4(t, path, 3, 3000)

	width, height, err := readSourceResolution(path)
	if err != nil {
		t.Fatalf("readSourceResolution: %v", err)
	}
	if width != 128 || height != 96 {
		t.Errorf("got %dx%d, want 128x96", width, height)
	}
}

func TestTranscodeSegment_SkipsIfAlreadySmall(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	writeSyntheticFMP4(t, src, 10, 3000) // 128x96 — well below 1280x720

	result, err := TranscodeSegment(src, 1280, 720)
	if err != nil {
		t.Fatalf("TranscodeSegment: %v", err)
	}
	if !result.Skipped {
		t.Error("expected skip=true for source smaller than target")
	}
	// No .tmp file should exist
	if _, statErr := os.Stat(src + ".tmp"); !os.IsNotExist(statErr) {
		t.Error("unexpected .tmp file left behind")
	}
}

func TestTranscodeSegment_OriginalUntouchedOnFailure(t *testing.T) {
	dir := t.TempDir()

	// Pass a non-existent path to trigger failure at open
	_, err := TranscodeSegment(filepath.Join(dir, "nonexistent.mp4"), 1280, 720)
	if err == nil {
		t.Fatal("expected error for non-existent file")
	}

	// No .tmp file should exist
	if _, statErr := os.Stat(filepath.Join(dir, "nonexistent.mp4.tmp")); !os.IsNotExist(statErr) {
		t.Error(".tmp file left behind after failure")
	}
}

// findTranscodeableClip searches the recording directories for a clip file that
// has enough fragments for transcoding tests. Returns the path, or "" if none found.
// The returned file has more than 4 fragments and is large enough to transcode.
func findTranscodeableClip(t *testing.T) string {
	t.Helper()
	dirs := []string{
		"../../recordings",
	}
	for _, base := range dirs {
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, cam := range entries {
			if !cam.IsDir() {
				continue
			}
			clipsDir := filepath.Join(base, cam.Name(), "clips")
			days, err := os.ReadDir(clipsDir)
			if err != nil {
				continue
			}
			for _, day := range days {
				if !day.IsDir() {
					continue
				}
				clips, err := os.ReadDir(filepath.Join(clipsDir, day.Name()))
				if err != nil {
					continue
				}
				for _, clip := range clips {
					if clip.IsDir() {
						continue
					}
					path := filepath.Join(clipsDir, day.Name(), clip.Name())
					f, err := os.Open(path)
					if err != nil {
						continue
					}
					_, frags, _, err := indexFile(f)
					f.Close()
					if err == nil && len(frags) >= 4 {
						return path
					}
				}
			}
		}
	}
	return ""
}

// copyClipToTemp copies a recording clip to a temp dir and returns the new path.
func copyClipToTemp(t *testing.T, src string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "clip.mp4")
	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open clip: %v", err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		t.Fatalf("copy clip: %v", err)
	}
	return dst
}

func TestTranscodeSegment_ReducesResolution(t *testing.T) {
	if !OpenH264Available() {
		t.Skip("OpenH264 not available")
	}
	clipPath := findTranscodeableClip(t)
	if clipPath == "" {
		t.Skip("no real recording clips available for transcoding test")
	}

	// Determine source resolution so we can pick a target that triggers transcoding
	srcW, srcH, err := readSourceResolution(clipPath)
	if err != nil {
		t.Fatalf("readSourceResolution: %v", err)
	}

	// Target half the resolution in each dimension (always > 25% area reduction)
	targetW := (srcW / 2) &^ 1
	targetH := (srcH / 2) &^ 1
	if targetW < 2 || targetH < 2 {
		t.Skipf("clip %dx%d too small to halve", srcW, srcH)
	}

	src := copyClipToTemp(t, clipPath)

	origStat, err := os.Stat(src)
	if err != nil {
		t.Fatalf("stat src: %v", err)
	}

	result, err := isolatedTranscodeSegment(t, src, targetW, targetH)
	if err != nil {
		t.Fatalf("TranscodeSegment: %v", err)
	}
	if result.Skipped {
		t.Fatalf("expected segment to be transcoded, not skipped (src %dx%d, target %dx%d)", srcW, srcH, targetW, targetH)
	}

	// Output replaces the original file in-place; verify it's smaller
	newStat, err := os.Stat(src)
	if err != nil {
		t.Fatalf("stat output: %v", err)
	}
	if newStat.Size() >= origStat.Size() {
		t.Errorf("output size %d >= original %d, expected reduction", newStat.Size(), origStat.Size())
	}

	// Output resolution must be at most targetW × targetH
	outW, outH, err := readSourceResolution(src)
	if err != nil {
		t.Fatalf("readSourceResolution on output: %v", err)
	}
	if outW > targetW || outH > targetH {
		t.Errorf("output resolution %dx%d exceeds target %dx%d", outW, outH, targetW, targetH)
	}
}

func TestTranscodeSegment_OutputParseable(t *testing.T) {
	if !OpenH264Available() {
		t.Skip("OpenH264 not available")
	}
	clipPath := findTranscodeableClip(t)
	if clipPath == "" {
		t.Skip("no real recording clips available for transcoding test")
	}

	srcW, srcH, err := readSourceResolution(clipPath)
	if err != nil {
		t.Fatalf("readSourceResolution: %v", err)
	}
	targetW := (srcW / 2) &^ 1
	targetH := (srcH / 2) &^ 1
	if targetW < 2 || targetH < 2 {
		t.Skipf("clip %dx%d too small to halve", srcW, srcH)
	}

	src := copyClipToTemp(t, clipPath)

	result, err := isolatedTranscodeSegment(t, src, targetW, targetH)
	if err != nil {
		t.Fatalf("TranscodeSegment: %v", err)
	}
	if result.Skipped {
		t.Fatal("expected segment to be transcoded, not skipped")
	}

	// Output must be indexable with non-empty init boxes and fragments
	f, err := os.Open(src)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	defer f.Close()

	initBoxes, frags, _, err := indexFile(f)
	if err != nil {
		t.Fatalf("indexFile on output: %v", err)
	}
	if len(initBoxes) == 0 {
		t.Error("no init boxes in output")
	}
	if len(frags) == 0 {
		t.Error("no fragments in output")
	}
}

func TestTranscodeSegment_AudioTrackPreserved(t *testing.T) {
	if !OpenH264Available() {
		t.Skip("OpenH264 not available")
	}
	clipPath := findTranscodeableClip(t)
	if clipPath == "" {
		t.Skip("no real recording clips available for transcoding test")
	}

	srcW, srcH, err := readSourceResolution(clipPath)
	if err != nil {
		t.Fatalf("readSourceResolution: %v", err)
	}
	targetW := (srcW / 2) &^ 1
	targetH := (srcH / 2) &^ 1
	if targetW < 2 || targetH < 2 {
		t.Skipf("clip %dx%d too small to halve", srcW, srcH)
	}

	// Count tracks in the source init
	var srcAudioTracks int
	{
		f, err := os.Open(clipPath)
		if err != nil {
			t.Fatalf("open clip: %v", err)
		}
		var srcInit fmp4.Init
		if err := srcInit.Unmarshal(f); err == nil {
			for _, tr := range srcInit.Tracks {
				if _, isH264 := tr.Codec.(*codecs.H264); !isH264 {
					srcAudioTracks++
				}
			}
		}
		f.Close()
	}

	src := copyClipToTemp(t, clipPath)

	result, err := isolatedTranscodeSegment(t, src, targetW, targetH)
	if err != nil {
		t.Fatalf("TranscodeSegment: %v", err)
	}
	if result.Skipped {
		t.Fatal("expected transcoding, not skip")
	}

	// Verify output init has the same number of audio tracks as source
	f, err := os.Open(src)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	defer f.Close()
	var outInit fmp4.Init
	if err := outInit.Unmarshal(f); err != nil {
		t.Fatalf("unmarshal output init: %v", err)
	}
	var outAudioTracks int
	for _, tr := range outInit.Tracks {
		if _, isH264 := tr.Codec.(*codecs.H264); !isH264 {
			outAudioTracks++
		}
	}
	if outAudioTracks != srcAudioTracks {
		t.Errorf("output has %d audio tracks, want %d (same as source)", outAudioTracks, srcAudioTracks)
	}
}
