package h264au

import (
	"bytes"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
)

// h264LevelLimit is one row of H.264 Table A-1: the per-level ceilings this
// code needs to pick the lowest level a stream legitimately requires.
type h264LevelLimit struct {
	idc       uint8
	maxMBPS   int // max macroblocks per second
	maxFS     int // max frame size in macroblocks
	maxDpbMbs int // max decoded-picture-buffer size in macroblocks
}

// h264LevelLimits is H.264 Table A-1 in ascending level order.
var h264LevelLimits = []h264LevelLimit{
	{10, 1485, 99, 396}, {11, 3000, 396, 900}, {12, 6000, 396, 2376}, {13, 11880, 396, 2376},
	{20, 11880, 396, 2376}, {21, 19800, 792, 4752}, {22, 20250, 1620, 8100}, {30, 40500, 1620, 8100},
	{31, 108000, 3600, 18000}, {32, 216000, 5120, 20480}, {40, 245760, 8192, 32768}, {41, 245760, 8192, 32768},
	{42, 522240, 8704, 34816}, {50, 589824, 22080, 110400}, {51, 983040, 36864, 184320}, {52, 2073600, 36864, 184320},
	{60, 4177920, 139264, 696320}, {61, 8355840, 139264, 696320}, {62, 16711680, 139264, 696320},
}

// minH264Level returns the lowest H.264 level idc that satisfies a stream's
// frame size (maxFS), macroblock rate (maxMBPS) and decode-buffer need
// (maxDpbMbs must hold refFrames pictures). It never returns a level the
// stream would exceed, so it is always safe to declare. Falls back to the
// highest table entry if nothing fits.
func minH264Level(frameMbs, fps, refFrames int) uint8 {
	if refFrames < 1 {
		refFrames = 1
	}
	for _, l := range h264LevelLimits {
		if l.maxFS >= frameMbs && l.maxMBPS >= frameMbs*fps && l.maxDpbMbs >= refFrames*frameMbs {
			return l.idc
		}
	}
	return h264LevelLimits[len(h264LevelLimits)-1].idc
}

// ClampSPSLevel rewrites an SPS's level_idc down to the lowest H.264 level the
// stream actually requires for its resolution, frame rate and reference-frame
// count.
//
// Cameras (notably Reolink) stamp a tiny sub-stream with an inflated level
// (e.g. 5.1 on 640x480). When the SPS omits an explicit max_dec_frame_buffering
// (VUI bitstream_restriction), iOS infers the decode-picture-buffer size from
// the level - 16 frames at level 5.1 versus ~6 at the correct level - and
// AVPlayer stalls for several seconds filling that buffer before native HLS
// playback starts. Declaring the real level shrinks the inferred buffer so
// playback starts immediately. This helps any over-declaring camera.
//
// It only ever lowers the level, never below what the bitstream requires, and
// leaves a correctly authored SPS unchanged (same backing array). It returns
// the input unchanged when the SPS cannot be parsed, or when the frame rate is
// not declared (without it a lower level cannot be proven safe), so it never
// produces an init segment that advertises constraints the stream exceeds.
func ClampSPSLevel(sps []byte) []byte {
	if len(sps) < 4 {
		return sps
	}
	var parsed h264.SPS
	if err := parsed.Unmarshal(sps); err != nil {
		return sps
	}

	// Require a declared frame rate: without it the macroblock-rate check
	// cannot be verified and a lower level might under-declare the stream.
	fps := spsFrameRate(&parsed)
	if fps <= 0 {
		return sps
	}

	frameMbs := (int(parsed.PicWidthInMbsMinus1) + 1) * (int(parsed.PicHeightInMapUnitsMinus1) + 1)
	if !parsed.FrameMbsOnlyFlag {
		frameMbs *= 2 // field coding stores two map units per coded frame
	}
	if frameMbs <= 0 {
		return sps
	}

	minLevel := minH264Level(frameMbs, fps, int(parsed.MaxNumRefFrames))

	// level_idc is a raw byte at a fixed offset (after the NAL header,
	// profile_idc and constraint-set flags), ahead of any RBSP emulation-
	// prevention bytes, so it can be rewritten in place without re-encoding.
	if sps[3] <= minLevel {
		return sps
	}
	out := make([]byte, len(sps))
	copy(out, sps)
	out[3] = minLevel
	return out
}

// ReplaceSPS makes an emitted keyframe agree with the SPS embedded
// in the fMP4 init segment. A normalized init paired with the camera's raw,
// over-declared in-band SPS makes strict decoders (notably VideoToolbox)
// reject the video track while continuing to advance the audio timeline.
// The outer slice is copied lazily and NAL payloads are never mutated.
func ReplaceSPS(au [][]byte, muxSPS []byte) [][]byte {
	if len(muxSPS) == 0 {
		return au
	}
	var out [][]byte
	for i, nalu := range au {
		if len(nalu) == 0 || h264.NALUType(nalu[0]&0x1F) != h264.NALUTypeSPS || bytes.Equal(nalu, muxSPS) {
			continue
		}
		if out == nil {
			out = append([][]byte(nil), au...)
		}
		out[i] = muxSPS
	}
	if out != nil {
		return out
	}
	return au
}

// spsFrameRate returns the frame rate declared in the SPS VUI timing info, or
// 0 when it is absent or implausible.
func spsFrameRate(s *h264.SPS) int {
	if s.VUI == nil || s.VUI.TimingInfo == nil || s.VUI.TimingInfo.NumUnitsInTick == 0 {
		return 0
	}
	fps := int(s.VUI.TimingInfo.TimeScale / (2 * s.VUI.TimingInfo.NumUnitsInTick))
	if fps <= 0 || fps > 240 {
		return 0
	}
	return fps
}
