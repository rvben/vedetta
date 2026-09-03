package media

import (
	"image"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	openh264 "github.com/y9o/go-openh264"
)

// synthI420 builds a solid-luma I420 frame of the given size, the smallest
// input that makes OpenH264 produce a real IDR access unit.
func synthI420(w, h int) *image.YCbCr {
	img := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	for i := range img.Y {
		img.Y[i] = uint8(i % 200)
	}
	for i := range img.Cb {
		img.Cb[i] = 128
		img.Cr[i] = 128
	}
	return img
}

// encodeResult is what an inspection of a single encode can carry away. The
// library-owned NAL buffers are not part of it: they are freed when the
// encoder is destroyed, so anything derived from them must be read inside
// encodeOneFrameIntoOversizedBuffers' callback.
type encodeResult struct {
	picBuf, infoBuf []byte
	picHigh         int // highest byte the library wrote past our picture struct, or -1
	infoHigh        int // highest byte the library wrote in the info buffer
	nalBytesSummed  int32
}

// encodeOneFrameIntoOversizedBuffers runs a single EncodeFrame with buffers far
// larger than the sizes transcodeFile declares, so the caller can see whether
// OpenH264 wrote outside the declared region. inspect runs while the encoder is
// still alive, which is the only window in which the layer NAL pointers are
// valid.
func encodeOneFrameIntoOversizedBuffers(t *testing.T, w, h int, inspect func(*cFrameBSInfo) int32) encodeResult {
	t.Helper()
	ensureOpenH264ForTest(t)

	OpenH264Lock()
	var enc *openh264.ISVCEncoder
	if ret := openh264.WelsCreateSVCEncoder(&enc); ret != 0 || enc == nil {
		OpenH264Unlock()
		t.Fatalf("WelsCreateSVCEncoder failed: %d", ret)
	}
	OpenH264Unlock()
	defer func() {
		OpenH264Lock()
		openh264.WelsDestroySVCEncoder(enc)
		OpenH264Unlock()
	}()

	param := openh264.SEncParamBase{
		IUsageType:     openh264.CAMERA_VIDEO_REAL_TIME,
		IPicWidth:      int32(w),
		IPicHeight:     int32(h),
		ITargetBitrate: targetBitrate(w, h),
		FMaxFrameRate:  15,
	}
	OpenH264Lock()
	if r := enc.Initialize(&param); r != 0 {
		OpenH264Unlock()
		t.Fatalf("encoder Initialize failed: %d", r)
	}
	OpenH264Unlock()
	defer func() {
		OpenH264Lock()
		enc.Uninitialize()
		OpenH264Unlock()
	}()

	// 32 KB each: several times any plausible struct size, so an overflow
	// lands inside the buffer where the test can see it instead of
	// corrupting unrelated memory.
	const oversized = 32 * 1024
	picBuf := make([]byte, oversized)
	infoBuf := make([]byte, oversized)

	img := synthI420(w, h)
	pic := asSourcePicture(picBuf)
	pic.iColorFormat = openh264.VideoFormatI420
	pic.iPicWidth = int32(w)
	pic.iPicHeight = int32(h)
	pic.iStride[0] = int32(img.YStride)
	pic.iStride[1] = int32(img.CStride)
	pic.iStride[2] = int32(img.CStride)
	pic.pData[0] = &img.Y[0]
	pic.pData[1] = &img.Cb[0]
	pic.pData[2] = &img.Cr[0]

	info := asFrameBSInfo(infoBuf)

	OpenH264Lock()
	ret := enc.EncodeFrame(
		(*openh264.SSourcePicture)(unsafe.Pointer(pic)),
		(*openh264.SFrameBSInfo)(unsafe.Pointer(info)))
	OpenH264Unlock()
	if ret != openh264.CmResultSuccess {
		t.Fatalf("EncodeFrame returned %d", ret)
	}

	res := encodeResult{picBuf: picBuf, infoBuf: infoBuf}
	if inspect != nil {
		res.nalBytesSummed = inspect(info)
	}

	// The source picture fields we wrote ourselves are not evidence of a
	// library write, so measure the picture buffer only past our own
	// writes; the info buffer we handed over zeroed.
	res.picHigh = highestNonZero(picBuf[sourcePictureSize:])
	if res.picHigh >= 0 {
		res.picHigh += sourcePictureSize
	}
	res.infoHigh = highestNonZero(infoBuf)
	return res
}

func highestNonZero(b []byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] != 0 {
			return i
		}
	}
	return -1
}

// TestEncodeFrameWritesNothingPastDeclaredBufferSizes is the memory-safety
// canary for the encoder ABI. transcodeFile allocates its EncodeFrame output
// struct as a fixed-size byte array; if that size is smaller than the struct
// the installed OpenH264 actually writes, every recompressed segment scribbles
// past the end of a Go allocation.
func TestEncodeFrameWritesNothingPastDeclaredBufferSizes(t *testing.T) {
	res := encodeOneFrameIntoOversizedBuffers(t, 320, 240, nil)

	if res.infoHigh >= frameBSInfoSize {
		t.Errorf("OpenH264 wrote to offset %d of SFrameBSInfo, %d bytes past the %d-byte buffer transcodeFile allocates",
			res.infoHigh, res.infoHigh-frameBSInfoSize+1, frameBSInfoSize)
	}
	if res.picHigh >= 0 {
		t.Errorf("OpenH264 wrote to offset %d of SSourcePicture, past the %d-byte buffer transcodeFile allocates",
			res.picHigh, sourcePictureSize)
	}
}

// TestFrameBSInfoTrailerIsReadableAtDeclaredOffset checks the frame-level
// trailer that follows the layer array. iFrameSizeInBytes is the encoder's own
// total for the frame, so it must equal the bytes summed across every layer.
// A wrong layer stride puts the trailer somewhere else and this reads zero.
func TestFrameBSInfoTrailerIsReadableAtDeclaredOffset(t *testing.T) {
	res := encodeOneFrameIntoOversizedBuffers(t, 320, 240, func(info *cFrameBSInfo) int32 {
		var summed int32
		for i := 0; i < info.layerCount(); i++ {
			layer := info.sLayerInfo[i]
			if layer.iNalCount <= 0 || layer.pNalLengthInByte == 0 {
				continue
			}
			for n := int32(0); n < layer.iNalCount; n++ {
				summed += *(*int32)(unsafe.Pointer(layer.pNalLengthInByte + uintptr(n)*4)) //nolint:govet // unsafeptr: library-owned array, valid until the next EncodeFrame
			}
		}
		return summed
	})
	info := asFrameBSInfo(res.infoBuf)

	if n := info.layerCount(); n <= 0 {
		t.Fatalf("encoder reported %d layers", n)
	}
	// Without the >0 guard a wrong layer stride passes vacuously: the sum
	// comes out zero and so does a trailer read from the wrong offset.
	if res.nalBytesSummed <= 0 {
		t.Fatalf("layers summed to %d bytes; the layer array is not being read correctly", res.nalBytesSummed)
	}
	if info.iFrameSizeInBytes != res.nalBytesSummed {
		t.Errorf("iFrameSizeInBytes is %d but the layers sum to %d bytes; the declared layout does not match the library",
			info.iFrameSizeInBytes, res.nalBytesSummed)
	}
	if info.eFrameType != openh264.VideoFrameTypeIDR {
		t.Errorf("eFrameType is %d, want IDR (%d) for the first encoded frame",
			info.eFrameType, openh264.VideoFrameTypeIDR)
	}
}

// blockVideoSampleCounts parses an fMP4 file and returns, per moof block that
// carries video, the number of video samples and whether each is a sync sample.
func blockVideoSampleCounts(t *testing.T, path string) (blocksWithVideo int, samples []bool) {
	t.Helper()
	src, err := openTranscodeSource(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer src.file.Close()

	for i, blk := range src.blocks {
		parts, err := readGOPBlock(src.file, blk, i)
		if err != nil {
			t.Fatalf("read block %d of %s: %v", i, path, err)
		}
		found := false
		for _, part := range parts {
			for _, tr := range part.Tracks {
				if tr.ID != src.videoTrackID {
					continue
				}
				for _, s := range tr.Samples {
					found = true
					samples = append(samples, !s.IsNonSyncSample)
				}
			}
		}
		if found {
			blocksWithVideo++
		}
	}
	return blocksWithVideo, samples
}

// transcodeFixtureCopy transcodes a scratch copy of the committed fixture and
// returns the path to the recompressed file.
func transcodeFixtureCopy(t *testing.T) string {
	t.Helper()
	ensureOpenH264ForTest(t)

	fixture := filepath.Join("testdata", "sample_segment.mp4")
	src, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "in.mp4")
	if err := os.WriteFile(in, src, 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.mp4")
	if err := transcodeFile(in, out, 1280, 720); err != nil {
		t.Fatalf("transcodeFile: %v", err)
	}
	return out
}

// TestTranscodeEncodesEveryGOP pins the headline defect: keyframe-only
// recompression writes exactly one picture per source GOP, so dropping any GOP
// silently deletes a second of video from the archive.
func TestTranscodeEncodesEveryGOP(t *testing.T) {
	ensureOpenH264ForTest(t)
	srcBlocks, _ := blockVideoSampleCounts(t, filepath.Join("testdata", "sample_segment.mp4"))
	if srcBlocks == 0 {
		t.Fatal("fixture has no video blocks")
	}

	_, outSamples := blockVideoSampleCounts(t, transcodeFixtureCopy(t))
	if len(outSamples) != srcBlocks {
		t.Errorf("recompressed %d of %d source GOPs; %d pictures were dropped",
			len(outSamples), srcBlocks, srcBlocks-len(outSamples))
	}
}

// TestTranscodeMarksEveryFrameAsSyncSample follows from keyframe-only
// recompression: every output picture is an IDR, so none may be flagged as a
// non-sync sample. A player that honours the flag cannot seek to a file whose
// samples all claim to depend on a predecessor that was never written.
func TestTranscodeMarksEveryFrameAsSyncSample(t *testing.T) {
	_, outSamples := blockVideoSampleCounts(t, transcodeFixtureCopy(t))
	if len(outSamples) == 0 {
		t.Fatal("no output samples")
	}
	nonSync := 0
	for _, isSync := range outSamples {
		if !isSync {
			nonSync++
		}
	}
	if nonSync != 0 {
		t.Errorf("%d of %d recompressed pictures are flagged non-sync, but every one is an IDR",
			nonSync, len(outSamples))
	}
}
