package media

import (
	"fmt"
	"image"
	"io"
	"log/slog"
	"os"
	"runtime"
	"unsafe"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	openh264 "github.com/y9o/go-openh264"
)

// scaleYCbCr scales a YCbCr I420 image to fit within (targetW, targetH) while
// preserving aspect ratio. Output dimensions are always even (required by H264).
// Uses nearest-neighbour sampling — sufficient for downscaling security footage.
func scaleYCbCr(src *image.YCbCr, targetW, targetH int) *image.YCbCr {
	srcW := src.Rect.Dx()
	srcH := src.Rect.Dy()

	// Compute scale to fit within target box, preserve aspect ratio
	scaleW := float64(targetW) / float64(srcW)
	scaleH := float64(targetH) / float64(srcH)
	scale := scaleW
	if scaleH < scaleW {
		scale = scaleH
	}

	outW := int(float64(srcW)*scale/2) * 2 // round down to even
	outH := int(float64(srcH)*scale/2) * 2

	if outW <= 0 {
		outW = 2
	}
	if outH <= 0 {
		outH = 2
	}

	dst := image.NewYCbCr(image.Rect(0, 0, outW, outH), image.YCbCrSubsampleRatio420)

	for dy := range outH {
		sy := dy * srcH / outH
		for dx := range outW {
			sx := dx * srcW / outW
			dst.Y[dy*dst.YStride+dx] = src.Y[sy*src.YStride+sx]
		}
	}
	// Chroma planes (half resolution for I420)
	for dy := range outH / 2 {
		sy := dy * (srcH / 2) / (outH / 2)
		for dx := range outW / 2 {
			sx := dx * (srcW / 2) / (outW / 2)
			dst.Cb[dy*dst.CStride+dx] = src.Cb[sy*src.CStride+sx]
			dst.Cr[dy*dst.CStride+dx] = src.Cr[sy*src.CStride+sx]
		}
	}

	return dst
}

// encoderInputValid reports whether the scaled I420 planes are large enough
// for the geometry handed to the C OpenH264 encoder.
//
// EncodeFrame receives the plane base pointers plus IStride and IPicHeight,
// then reads IPicHeight rows of IStride[0] luma bytes and IPicHeight/2 rows of
// IStride[1]/IStride[2] chroma bytes directly out of these Go-owned slices
// across the cgo boundary. If a degenerate or corrupt decoded frame produces a
// plane shorter than that traversal, the C library reads past Go heap memory -
// an out-of-bounds read invisible to Go's bounds checker that produces exactly
// the random-victim heap-fault signature behind the recompression crash loop.
// A well-formed scaleYCbCr result (even dimensions, exactly-sized planes)
// always passes; only a corrupt frame fails, and skipping a single
// recompressed GOP is invisible next to a heap fault.
func encoderInputValid(scaled *image.YCbCr, outW, outH int) bool {
	if outW <= 0 || outH <= 0 || outW%2 != 0 || outH%2 != 0 {
		return false
	}
	cW := outW / 2
	cH := outH / 2
	if scaled.YStride < outW || scaled.CStride < cW {
		return false
	}
	if len(scaled.Y) < scaled.YStride*outH ||
		len(scaled.Cb) < scaled.CStride*cH ||
		len(scaled.Cr) < scaled.CStride*cH {
		return false
	}
	return true
}

// shouldTranscode reports whether transcoding from (srcW, srcH) to (targetW, targetH)
// is worth doing. Returns (skip=true, 0, 0) when the source is already at or below
// the target size, or when the area reduction is less than 25%.
// When skip=false, returns the actual output dimensions (aspect-ratio-corrected, even).
func shouldTranscode(srcW, srcH, targetW, targetH int) (skip bool, outW, outH int) {
	// Already at or below target in both dimensions
	if srcW <= targetW && srcH <= targetH {
		return true, 0, 0
	}

	// Compute output dimensions preserving aspect ratio
	scaleW := float64(targetW) / float64(srcW)
	scaleH := float64(targetH) / float64(srcH)
	scale := scaleW
	if scaleH < scaleW {
		scale = scaleH
	}
	outW = int(float64(srcW)*scale/2) * 2
	outH = int(float64(srcH)*scale/2) * 2
	if outW <= 0 {
		outW = 2
	}
	if outH <= 0 {
		outH = 2
	}

	// Skip if area reduction is less than 25%
	srcArea := srcW * srcH
	outArea := outW * outH
	if float64(outArea) >= float64(srcArea)*0.75 {
		return true, 0, 0
	}

	return false, outW, outH
}

// readSourceResolution reads the H264 video resolution from an fMP4 file
// by parsing the init segment and decoding the SPS from the H264 codec config.
func readSourceResolution(path string) (width, height int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	var init fmp4.Init
	if err := init.Unmarshal(f); err != nil {
		return 0, 0, fmt.Errorf("unmarshal init: %w", err)
	}

	for _, track := range init.Tracks {
		h264Codec, ok := track.Codec.(*codecs.H264)
		if !ok {
			continue
		}
		var sps h264.SPS
		if err := sps.Unmarshal(h264Codec.SPS); err != nil {
			return 0, 0, fmt.Errorf("parse SPS: %w", err)
		}
		return sps.Width(), sps.Height(), nil
	}

	return 0, 0, fmt.Errorf("no H264 video track found in init segment")
}

// TranscodeResult holds the outcome of a TranscodeSegment call.
type TranscodeResult struct {
	OriginalSize int64 `json:"original_size"`
	NewSize      int64 `json:"new_size"`
	Skipped      bool  `json:"skipped"` // true when resolution check determined transcoding isn't worth it
}

// TranscodeResultMarker prefixes the single JSON line that the `vedetta
// transcode` subcommand writes to stdout. The recompressor runs that subcommand
// as a throwaway child process and scans its stdout for this marker, so the
// result is recovered reliably even if the OpenH264 C library writes diagnostic
// lines to stdout alongside it.
const TranscodeResultMarker = "VEDETTA_TRANSCODE_RESULT:"

// TranscodeSegment re-encodes the video track of the fMP4 at path to (targetW, targetH),
// copying audio verbatim. Writes output to path+".tmp", verifies it, then atomically
// renames it over path. If the segment is already small enough (resolution check),
// returns TranscodeResult{Skipped: true} without touching the file.
func TranscodeSegment(path string, targetW, targetH int) (TranscodeResult, error) {
	return TranscodeSegmentTo(path, path, targetW, targetH)
}

// TranscodeSegmentTo re-encodes sourcePath into outputPath. When the paths
// differ, sourcePath is not modified. The output is written to
// outputPath+".tmp", verified, and then atomically renamed into place. Keeping
// source and output separate lets a supervising process commit the result only
// after the worker exits cleanly. If the source is already small enough, no
// output file is created.
func TranscodeSegmentTo(sourcePath, outputPath string, targetW, targetH int) (TranscodeResult, error) {
	srcW, srcH, err := readSourceResolution(sourcePath)
	if err != nil {
		return TranscodeResult{}, fmt.Errorf("read source resolution: %w", err)
	}

	skip, outW, outH := shouldTranscode(srcW, srcH, targetW, targetH)
	if skip {
		return TranscodeResult{Skipped: true}, nil
	}

	origInfo, err := os.Stat(sourcePath)
	if err != nil {
		return TranscodeResult{}, fmt.Errorf("stat source: %w", err)
	}
	origSize := origInfo.Size()

	tmpPath := outputPath + ".tmp"
	if err := transcodeFile(sourcePath, tmpPath, outW, outH); err != nil {
		_ = os.Remove(tmpPath)
		return TranscodeResult{}, fmt.Errorf("transcode: %w", err)
	}

	if err := verifyFMP4(tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return TranscodeResult{}, fmt.Errorf("verify output: %w", err)
	}

	newInfo, err := os.Stat(tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return TranscodeResult{}, fmt.Errorf("stat output: %w", err)
	}
	newSize := newInfo.Size()

	if err := os.Rename(tmpPath, outputPath); err != nil {
		_ = os.Remove(tmpPath)
		return TranscodeResult{}, fmt.Errorf("rename: %w", err)
	}

	return TranscodeResult{OriginalSize: origSize, NewSize: newSize}, nil
}

func verifyFMP4(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, frags, _, err := indexFile(f)
	if err != nil {
		return fmt.Errorf("index: %w", err)
	}
	if len(frags) == 0 {
		return fmt.Errorf("output has no fragments")
	}
	return nil
}

// ValidateFMP4 verifies that path is a readable fragmented MP4 containing at
// least one media fragment. Supervising processes use it to independently
// validate worker output before replacing a recording.
func ValidateFMP4(path string) error {
	return verifyFMP4(path)
}

// moofBlock is a deduplicated moof+mdat block location. indexFile emits one
// fragment entry per traf, but multi-track moofs share a single mdat; a block
// captures each unique moof+mdat pair so it is parsed exactly once.
type moofBlock struct {
	moofOffset int64
	moofSize   int64
	mdatOffset int64
	mdatSize   int64
}

// identifyTracks finds the H264 video track and the (optional) audio track in
// an fMP4 init segment. The first non-H264 track is treated as audio.
func identifyTracks(srcInit fmp4.Init) (videoTrackID, audioTrackID int, h264Codec *codecs.H264, err error) {
	for _, tr := range srcInit.Tracks {
		if c, ok := tr.Codec.(*codecs.H264); ok {
			videoTrackID = tr.ID
			h264Codec = c
		} else {
			audioTrackID = tr.ID
		}
	}
	if videoTrackID == 0 || h264Codec == nil {
		return 0, 0, nil, fmt.Errorf("no H264 video track in source")
	}
	return videoTrackID, audioTrackID, h264Codec, nil
}

// collectMoofBlocks builds the deduplicated list of moof+mdat blocks from the
// indexed fragments, skipping empty mdats and trafs that share an already-seen
// moof offset.
func collectMoofBlocks(frags []fragment) []moofBlock {
	seen := make(map[int64]bool)
	var blocks []moofBlock
	for _, f := range frags {
		if f.mdatSize == 0 || seen[f.moofOffset] {
			continue
		}
		seen[f.moofOffset] = true
		blocks = append(blocks, moofBlock{
			moofOffset: f.moofOffset,
			moofSize:   f.moofSize,
			mdatOffset: f.mdatOffset,
			mdatSize:   f.mdatSize,
		})
	}
	return blocks
}

// computeVideoFPS derives the frame rate from the first video fragment's sample
// duration so the encoder's rate control matches non-30fps sources. Implausible
// or missing values fall back to 15fps.
func computeVideoFPS(frags []fragment, videoTrackID int, videoTS uint32) float32 {
	fps := float32(15)
	for _, f := range frags {
		if t := f.traf(uint32(videoTrackID)); t != nil && t.duration > 0 {
			fps = float32(videoTS) / float32(t.duration)
			break
		}
	}
	if fps <= 0 || fps > 60 {
		fps = 15
	}
	return fps
}

// extractIDRAccessUnit builds an Annex B access unit containing only the SPS,
// PPS, and IDR NALs from annexB, each prefixed with a 4-byte start code.
// P-frames and all other NAL types are discarded: the recompressed file stores
// one keyframe per GOP, so decoding 30+ frames just to drop them is avoided.
func extractIDRAccessUnit(annexB []byte) []byte {
	startCode := []byte{0, 0, 0, 1}
	var idrAU []byte
	for _, nal := range splitAnnexB(annexB) {
		if len(nal) == 0 {
			continue
		}
		switch nal[0] & 0x1f {
		case 7, 8, 5: // SPS, PPS, IDR
			idrAU = append(idrAU, startCode...)
			idrAU = append(idrAU, nal...)
		}
	}
	return idrAU
}

// buildOutputInit assembles the output init segment: a video track carrying the
// re-encoded SPS/PPS, followed by a verbatim copy of the source audio track
// when one is present.
func buildOutputInit(srcInit fmp4.Init, videoTrackID, audioTrackID int, videoTS uint32, outSPS, outPPS []byte) fmp4.Init {
	outInit := fmp4.Init{
		Tracks: []*fmp4.InitTrack{
			{ID: videoTrackID, TimeScale: videoTS, Codec: &codecs.H264{SPS: outSPS, PPS: outPPS}},
		},
	}
	if audioTrackID != 0 {
		for _, tr := range srcInit.Tracks {
			if tr.ID == audioTrackID {
				outInit.Tracks = append(outInit.Tracks, &fmp4.InitTrack{
					ID:        tr.ID,
					TimeScale: tr.TimeScale,
					Codec:     tr.Codec,
				})
				break
			}
		}
	}
	return outInit
}

// transcodeSource is the parsed shape of an input segment: everything the
// re-encode needs to read before any codec is created. Separating it keeps the
// file and container handling away from the encoder loop, where the pinned
// C buffers make every edit delicate.
type transcodeSource struct {
	file         *os.File
	init         fmp4.Init
	videoTrackID int
	audioTrackID int
	h264         *codecs.H264
	frags        []fragment
	timeScales   map[uint32]uint32
	blocks       []moofBlock
}

// openTranscodeSource opens src and reads its init segment and fragment index.
// The caller owns the returned file and must close it.
func openTranscodeSource(src string) (*transcodeSource, error) {
	in, err := os.Open(src)
	if err != nil {
		return nil, fmt.Errorf("open src: %w", err)
	}

	ts := &transcodeSource{file: in}
	if err := ts.init.Unmarshal(in); err != nil {
		in.Close()
		return nil, fmt.Errorf("unmarshal init: %w", err)
	}

	ts.videoTrackID, ts.audioTrackID, ts.h264, err = identifyTracks(ts.init)
	if err != nil {
		in.Close()
		return nil, err
	}

	// The init parse consumed the header, so rewind before indexing the
	// moof+mdat pairs.
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		in.Close()
		return nil, err
	}
	_, ts.frags, ts.timeScales, err = indexFile(in)
	if err != nil {
		in.Close()
		return nil, fmt.Errorf("index: %w", err)
	}

	ts.blocks = collectMoofBlocks(ts.frags)
	return ts, nil
}

// readGOPBlock reads one moof+mdat pair whole and unmarshals it as fmp4 parts,
// which separates per-track sample payloads regardless of whether the moof
// carries one traf or a video and audio pair.
func readGOPBlock(in io.ReadSeeker, blk moofBlock, gopIdx int) (fmp4.Parts, error) {
	blockSize := (blk.mdatOffset - blk.moofOffset) + blk.mdatSize
	if _, err := in.Seek(blk.moofOffset, io.SeekStart); err != nil {
		return nil, err
	}
	blockBuf := make([]byte, blockSize)
	if _, err := io.ReadFull(in, blockBuf); err != nil {
		return nil, fmt.Errorf("read block %d: %w", gopIdx, err)
	}

	var parts fmp4.Parts
	if err := parts.Unmarshal(blockBuf); err != nil {
		return nil, fmt.Errorf("unmarshal block %d: %w", gopIdx, err)
	}
	return parts, nil
}

// gopTracks is one GOP split by track: the video payload concatenated for the
// decoder, and the audio samples copied for verbatim pass-through.
type gopTracks struct {
	videoAVCC     []byte
	videoBase     uint64
	videoDuration uint32
	audioSamples  []*fmp4.Sample
	audioBase     uint64
}

// splitGOPTracks separates one block's samples by track. Audio payloads are
// copied because they outlive the block buffer they were unmarshaled from and
// are written to the output as they are.
func splitGOPTracks(parts fmp4.Parts, videoTrackID, audioTrackID int) gopTracks {
	var g gopTracks
	for _, part := range parts {
		for _, tr := range part.Tracks {
			switch tr.ID {
			case videoTrackID:
				g.videoBase = tr.BaseTime
				for _, s := range tr.Samples {
					g.videoDuration += s.Duration
					g.videoAVCC = append(g.videoAVCC, s.Payload...)
				}
			case audioTrackID:
				g.audioBase = tr.BaseTime
				for _, s := range tr.Samples {
					g.audioSamples = append(g.audioSamples, &fmp4.Sample{
						Duration:        s.Duration,
						Payload:         append([]byte(nil), s.Payload...),
						IsNonSyncSample: s.IsNonSyncSample,
					})
				}
			}
		}
	}
	return g
}

// transcodeFile reads src fMP4, re-encodes video track at (outW, outH), copies audio verbatim, writes to dst.
func transcodeFile(src, dst string, outW, outH int) error {
	if !ensureOpenH264() {
		return fmt.Errorf("OpenH264 not available")
	}

	srcFile, err := openTranscodeSource(src)
	if err != nil {
		return err
	}
	defer srcFile.file.Close()

	in := srcFile.file
	srcInit := srcFile.init
	videoTrackID := srcFile.videoTrackID
	audioTrackID := srcFile.audioTrackID
	srcH264Codec := srcFile.h264
	indexedFrags := srcFile.frags
	trackTimeScales := srcFile.timeScales
	blocks := srcFile.blocks

	// Create H264 decoder
	dec := NewH264Decoder()
	if dec == nil {
		return fmt.Errorf("failed to create H264 decoder")
	}
	defer dec.Close()

	// Create H264 encoder
	OpenH264Lock()
	var ppEnc *openh264.ISVCEncoder
	if ret := openh264.WelsCreateSVCEncoder(&ppEnc); ret != 0 || ppEnc == nil {
		OpenH264Unlock()
		return fmt.Errorf("WelsCreateSVCEncoder failed: %d", ret)
	}
	OpenH264Unlock()
	defer func() {
		OpenH264Lock()
		openh264.WelsDestroySVCEncoder(ppEnc)
		OpenH264Unlock()
	}()

	videoTS := trackTimeScales[uint32(videoTrackID)]
	if videoTS == 0 {
		videoTS = 90000
	}
	fps := computeVideoFPS(indexedFrags, videoTrackID, videoTS)

	encParam := openh264.SEncParamBase{
		IUsageType:     openh264.CAMERA_VIDEO_REAL_TIME,
		IPicWidth:      int32(outW),
		IPicHeight:     int32(outH),
		ITargetBitrate: targetBitrate(outW, outH),
		FMaxFrameRate:  fps,
	}
	OpenH264Lock()
	if r := ppEnc.Initialize(&encParam); r != 0 {
		OpenH264Unlock()
		return fmt.Errorf("encoder Initialize failed: %d", r)
	}
	OpenH264Unlock()
	defer func() {
		OpenH264Lock()
		ppEnc.Uninitialize()
		OpenH264Unlock()
	}()

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create dst: %w", err)
	}
	defer out.Close()

	type encodedGOP struct {
		videoSamples []*fmp4.Sample
		audioSamples []*fmp4.Sample
		videoBase    uint64
		audioBase    uint64
		seqNum       uint32
	}

	var gops []encodedGOP
	var outSPS, outPPS []byte
	var seqNum uint32 = 1

	// GC-SAFE ENCODER I/O BUFFERS — DO NOT "SIMPLIFY" TO TYPED STRUCTS.
	//
	// OpenH264 writes C-owned pointers (PBsBuf, PNalLengthInByte) into the
	// SFrameBSInfo struct, and we write Go-owned Pinner-pinned pointers
	// (PData[0..2]) into SSourcePicture. If either struct is allocated as
	// its Go type, those fields appear in the GC pointer bitmap and the
	// concurrent GC (Go 1.26+) will scan them during marking.
	//
	// For SFrameBSInfo the scanned pointers are C-owned addresses that
	// look like they might be inside the Go heap; the GC calls findObject
	// on them and crashes with "found pointer to free object" or "found
	// bad pointer in Go heap", or the process gets SIGKILLed.
	//
	// By allocating the structs as [N]byte arrays, the GC bitmap for the
	// allocation is all zeros — no words are ever scanned as pointers, no
	// matter where the memory lives (stack or heap). We cast to the typed
	// pointer only for field writes; Go's GC scans allocations by their
	// original type metadata, not by what pointers later alias them.
	//
	// Regression test: TestTranscodeSegment_Fixture in transcoder_real_test.go.
	var encSrcPicBuf [unsafe.Sizeof(openh264.SSourcePicture{})]byte
	var encInfoBuf [unsafe.Sizeof(openh264.SFrameBSInfo{})]byte

	for gopIdx, blk := range blocks {
		parts, err := readGOPBlock(in, blk, gopIdx)
		if err != nil {
			return err
		}

		gop := splitGOPTracks(parts, videoTrackID, audioTrackID)
		videoAVCC := gop.videoAVCC
		videoBase := gop.videoBase
		videoDuration := gop.videoDuration
		newAudioSamples := gop.audioSamples
		audioBase := gop.audioBase

		if len(videoAVCC) == 0 {
			continue
		}

		// Convert AVCC samples to Annex B for the decoder
		annexB, err := avccToAnnexB(videoAVCC)
		if err != nil {
			continue
		}

		// Reorder NALs: SPS and PPS must precede slice data for the decoder
		// to initialise. Recordings may place SPS/PPS at the end of a GOP
		// (intended for the next GOP), so we extract them and always prepend
		// the init segment's SPS/PPS before all slice NALs.
		annexB = reorderAnnexBWithSPSPPS(annexB, srcH264Codec.SPS, srcH264Codec.PPS)

		var newVideoSamples []*fmp4.Sample
		pinner := &runtime.Pinner{}

		idrAU := extractIDRAccessUnit(annexB)
		if len(idrAU) == 0 {
			continue
		}

		frame := dec.Decode(idrAU)
		if frame == nil {
			frame = dec.Flush()
		}

		if frame != nil {
			scaled := scaleYCbCr(frame, outW, outH)

			// Defense-in-depth at the cgo encoder boundary: never hand the C
			// encoder a plane shorter than the IStride*IPicHeight it will
			// traverse. A short buffer here is an invisible out-of-bounds
			// read into foreign Go heap. The log line is the instrumentation:
			// in a healthy stream this never fires; a burst of it is the
			// smoking gun for the recompression-path corruptor and the
			// prerequisite signal before tiered storage may be re-enabled.
			if !encoderInputValid(scaled, outW, outH) {
				slog.Warn("transcode: skipping frame with invalid encoder input geometry",
					"outW", outW, "outH", outH,
					"yStride", scaled.YStride, "cStride", scaled.CStride,
					"lenY", len(scaled.Y), "lenCb", len(scaled.Cb), "lenCr", len(scaled.Cr),
					"gop", gopIdx)
				pinner.Unpin()
				continue
			}

			// Zero-clear the reused encoder I/O buffers before each use to ensure
			// no data from a previous iteration leaks into the new frame.
			for i := range encSrcPicBuf {
				encSrcPicBuf[i] = 0
			}
			for i := range encInfoBuf {
				encInfoBuf[i] = 0
			}
			encSrcPic := (*openh264.SSourcePicture)(unsafe.Pointer(&encSrcPicBuf[0]))
			encSrcPic.IColorFormat = openh264.VideoFormatI420
			encSrcPic.IPicWidth = int32(outW)
			encSrcPic.IPicHeight = int32(outH)
			encSrcPic.UiTimeStamp = int64(gopIdx * 1000)
			encSrcPic.IStride[0] = int32(scaled.YStride)
			encSrcPic.IStride[1] = int32(scaled.CStride)
			encSrcPic.IStride[2] = int32(scaled.CStride)
			pinner.Pin(&scaled.Y[0])
			pinner.Pin(&scaled.Cb[0])
			pinner.Pin(&scaled.Cr[0])
			encSrcPic.PData[0] = (*uint8)(unsafe.Pointer(&scaled.Y[0]))
			encSrcPic.PData[1] = (*uint8)(unsafe.Pointer(&scaled.Cb[0]))
			encSrcPic.PData[2] = (*uint8)(unsafe.Pointer(&scaled.Cr[0]))

			encInfo := (*openh264.SFrameBSInfo)(unsafe.Pointer(&encInfoBuf[0]))

			OpenH264Lock()
			encRet := ppEnc.EncodeFrame(encSrcPic, encInfo)
			// Extract the layer count and frame type while still inside the lock,
			// before the C-owned NAL buffers could be invalidated by another call.
			nLayers := int(encInfo.ILayerNum)
			encFrameType := encInfo.EFrameType
			const maxLayers = 128
			if nLayers > maxLayers {
				nLayers = maxLayers
			}
			// Capture the encoder's output buffer addresses as uintptr, not as
			// pointer-typed values. PBsBuf and PNalLengthInByte point into
			// memory owned by the OpenH264 library (the C library mallocs these
			// even in the purego/dlopen build), valid until the next
			// EncodeFrame or destroy call, both serialized by OpenH264Lock.
			// Holding such an address in a Go *uint8/*int32 would make the GC
			// trace it as a Go heap pointer and run span lookups on a foreign
			// address, which can fault at a wild address. uintptr is not traced
			// by the GC, so the address stays opaque to it.
			var (
				layerBufPtrs   [maxLayers]uintptr
				layerLenPtrs   [maxLayers]uintptr
				layerNalCounts [maxLayers]int32
			)
			for iLayer := 0; iLayer < nLayers; iLayer++ {
				layer := &encInfo.SLayerInfo[iLayer]
				layerBufPtrs[iLayer] = uintptr(unsafe.Pointer(layer.PBsBuf))
				layerLenPtrs[iLayer] = uintptr(unsafe.Pointer(layer.PNalLengthInByte))
				layerNalCounts[iLayer] = layer.INalCount
			}
			OpenH264Unlock()

			// Unpin after the encode call; the C library no longer needs the
			// Go-managed plane data. Do this before any further allocations to
			// release the pinning obligation promptly.
			pinner.Unpin()
			pinner = &runtime.Pinner{}

			// Copy NAL bytes from the library-owned buffers into Go-owned
			// slices. The buffers remain valid until the next EncodeFrame or
			// WelsDestroySVCEncoder call (serialized by OpenH264Lock above).
			//
			// Access the memory via uintptr arithmetic and transient
			// unsafe.Pointer conversions rather than unsafe.Slice, so no slice
			// header whose Data field holds a foreign address is ever live on
			// the Go stack or heap for the GC to trace.
			var nalBytes []byte
			if encRet == openh264.CmResultSuccess && encFrameType != openh264.VideoFrameTypeSkip {
				for iLayer := 0; iLayer < nLayers; iLayer++ {
					nalCount := layerNalCounts[iLayer]
					nalLenPtr := layerLenPtrs[iLayer]
					bufPtr := layerBufPtrs[iLayer]
					if nalCount <= 0 || nalLenPtr == 0 || bufPtr == 0 {
						continue
					}
					// Sum NAL unit lengths by stepping through the library-owned
					// int32 array one element at a time.
					//
					// vet's unsafeptr analyzer reports both conversions below
					// because it cannot tell a foreign address from a Go
					// pointer the collector may move. nalLenPtr and bufPtr name
					// memory OpenH264 allocated, which the collector neither
					// moves nor frees, and which stays valid until the next
					// EncodeFrame or destroy call. The suppression is per line
					// on purpose: golangci-lint runs the same analyzer and
					// still fails on any conversion that is not marked.
					var layerSize int32
					for i := int32(0); i < nalCount; i++ {
						l := *(*int32)(unsafe.Pointer(nalLenPtr + uintptr(i)*4)) //nolint:govet // unsafeptr: address held as uintptr; the GC must not trace library-owned memory
						layerSize += l
					}
					if layerSize <= 0 {
						continue
					}
					// Copy the NAL bytes one byte at a time into a Go-owned slice.
					layerCopy := make([]byte, layerSize)
					for i := int32(0); i < layerSize; i++ {
						layerCopy[i] = *(*uint8)(unsafe.Pointer(bufPtr + uintptr(i))) //nolint:govet // unsafeptr: address held as uintptr; the GC must not trace library-owned memory
					}
					nalBytes = append(nalBytes, layerCopy...)
				}
			}

			if encRet == openh264.CmResultSuccess && encFrameType != openh264.VideoFrameTypeSkip {

				if outSPS == nil {
					outSPS, outPPS = extractSPSPPS(nalBytes)
				}

				avccPayloadOut, err := annexBToAVCC(nalBytes)
				if err == nil && len(avccPayloadOut) > 0 {
					isNonSync := encFrameType != openh264.VideoFrameTypeIDR
					newVideoSamples = append(newVideoSamples, &fmp4.Sample{
						Duration:        videoDuration,
						Payload:         avccPayloadOut,
						IsNonSyncSample: isNonSync,
					})
				}
			}
		}
		pinner.Unpin()

		if len(newVideoSamples) > 0 {
			gops = append(gops, encodedGOP{
				videoSamples: newVideoSamples,
				audioSamples: newAudioSamples,
				videoBase:    videoBase,
				audioBase:    audioBase,
				seqNum:       seqNum,
			})
			seqNum++
		}
	}

	if len(gops) == 0 || outSPS == nil {
		return fmt.Errorf("no frames encoded successfully")
	}

	outInit := buildOutputInit(srcInit, videoTrackID, audioTrackID, videoTS, outSPS, outPPS)

	if err := outInit.Marshal(out); err != nil {
		return fmt.Errorf("write init: %w", err)
	}

	for _, gop := range gops {
		// Validate samples before marshalling — nil payloads panic the fmp4 library
		validVideo := make([]*fmp4.Sample, 0, len(gop.videoSamples))
		for _, s := range gop.videoSamples {
			if len(s.Payload) > 0 {
				validVideo = append(validVideo, s)
			}
		}
		if len(validVideo) == 0 {
			continue
		}

		tracks := []*fmp4.PartTrack{
			{ID: videoTrackID, BaseTime: gop.videoBase, Samples: validVideo},
		}
		if audioTrackID != 0 && len(gop.audioSamples) > 0 {
			validAudio := make([]*fmp4.Sample, 0, len(gop.audioSamples))
			for _, s := range gop.audioSamples {
				if len(s.Payload) > 0 {
					validAudio = append(validAudio, s)
				}
			}
			if len(validAudio) > 0 {
				tracks = append(tracks, &fmp4.PartTrack{
					ID:       audioTrackID,
					BaseTime: gop.audioBase,
					Samples:  validAudio,
				})
			}
		}
		part := fmp4.Part{SequenceNumber: gop.seqNum, Tracks: tracks}
		if err := part.Marshal(out); err != nil {
			return fmt.Errorf("write part %d: %w", gop.seqNum, err)
		}
	}

	return nil
}

func targetBitrate(w, h int) int32 {
	pixels := w * h
	switch {
	case pixels >= 1920*1080:
		return 2_000_000
	case pixels >= 1280*720:
		return 1_000_000
	case pixels >= 854*480:
		return 500_000
	default:
		return 300_000
	}
}

// reorderAnnexBWithSPSPPS ensures SPS and PPS precede all slice NALs.
// It strips any in-stream SPS/PPS and prepends the canonical ones from
// the init segment. This handles recordings where SPS/PPS appear after
// slice data (at the end of a GOP, intended for the next GOP).
func reorderAnnexBWithSPSPPS(annexB, sps, pps []byte) []byte {
	startCode := []byte{0, 0, 0, 1}
	var out []byte
	out = append(out, startCode...)
	out = append(out, sps...)
	out = append(out, startCode...)
	out = append(out, pps...)
	for _, nal := range splitAnnexB(annexB) {
		if len(nal) == 0 {
			continue
		}
		nalType := nal[0] & 0x1f
		if nalType == 7 || nalType == 8 {
			continue // skip in-stream SPS/PPS, we already prepended
		}
		out = append(out, startCode...)
		out = append(out, nal...)
	}
	return out
}

func avccToAnnexB(avcc []byte) ([]byte, error) {
	var out []byte
	pos := 0
	for pos+4 <= len(avcc) {
		nalLen := int(avcc[pos])<<24 | int(avcc[pos+1])<<16 | int(avcc[pos+2])<<8 | int(avcc[pos+3])
		pos += 4
		if pos+nalLen > len(avcc) {
			return nil, fmt.Errorf("AVCC NAL length %d exceeds buffer", nalLen)
		}
		out = append(out, 0, 0, 0, 1)
		out = append(out, avcc[pos:pos+nalLen]...)
		pos += nalLen
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no NAL units in AVCC payload")
	}
	return out, nil
}

func annexBToAVCC(annexB []byte) ([]byte, error) {
	nals := splitAnnexB(annexB)
	var out []byte
	for _, nal := range nals {
		if len(nal) == 0 {
			continue
		}
		nalType := nal[0] & 0x1f
		if nalType == 7 || nalType == 8 {
			continue // SPS/PPS go in init, not samples
		}
		l := len(nal)
		out = append(out, byte(l>>24), byte(l>>16), byte(l>>8), byte(l))
		out = append(out, nal...)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no non-SPS/PPS NAL units in Annex B data")
	}
	return out, nil
}

func extractSPSPPS(annexB []byte) (sps, pps []byte) {
	nals := splitAnnexB(annexB)
	for _, nal := range nals {
		if len(nal) == 0 {
			continue
		}
		switch nal[0] & 0x1f {
		case 7:
			sps = append([]byte(nil), nal...)
		case 8:
			pps = append([]byte(nil), nal...)
		}
	}
	return
}

func splitAnnexB(data []byte) [][]byte {
	var nals [][]byte
	i := 0
	for i < len(data) {
		startLen := 0
		if i+4 <= len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1 {
			startLen = 4
		} else if i+3 <= len(data) && data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			startLen = 3
		}
		if startLen > 0 {
			nals = append(nals, nil)
			i += startLen
			continue
		}
		if len(nals) > 0 {
			nals[len(nals)-1] = append(nals[len(nals)-1], data[i])
		}
		i++
	}
	return nals
}
