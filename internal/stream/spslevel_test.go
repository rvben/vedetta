package stream

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
)

// Real sub-stream SPS captured from production cameras.
const (
	// Reolink front_door: High profile, 640x480, level 5.1 (0x33) - absurdly
	// over-declared for the resolution, which is the bug this fix targets.
	frontDoorSPSHex = "67640033ac1514a0a03da1000004f6000063380400"
	// Tapo garage: High profile, 640x360, level 2.2 (0x16) - already minimal,
	// must be left untouched.
	garageSPSHex = "67640016acd20280bfe58400000fa40001d53810"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex fixture: %v", err)
	}
	return b
}

// A camera that stamps level 5.1 on a 640x480 sub-stream must be rewritten down
// to the real minimal level so iOS does not infer a 16-frame decode buffer and
// stall before native HLS playback. The rewrite must keep the SPS decodable.
func TestClampSPSLevel_LowersInflatedLevel(t *testing.T) {
	in := mustHex(t, frontDoorSPSHex)
	if in[3] != 0x33 {
		t.Fatalf("fixture should declare level 5.1 (0x33), got 0x%02x", in[3])
	}

	out := clampSPSLevel(in)
	if out[3] >= 0x33 {
		t.Fatalf("level not lowered: 0x%02x", out[3])
	}
	if out[3] < 22 {
		t.Fatalf("level lowered below the resolution minimum (level 2.2): %d", out[3])
	}

	var sps h264.SPS
	if err := sps.Unmarshal(out); err != nil {
		t.Fatalf("clamped SPS no longer decodes: %v", err)
	}
	if sps.LevelIdc != out[3] {
		t.Fatalf("parsed level %d disagrees with byte 0x%02x", sps.LevelIdc, out[3])
	}

	// Resolution must be preserved exactly - only the level byte changes.
	orig := mustHex(t, frontDoorSPSHex)
	var before h264.SPS
	if err := before.Unmarshal(orig); err != nil {
		t.Fatalf("fixture SPS does not decode: %v", err)
	}
	if sps.PicWidthInMbsMinus1 != before.PicWidthInMbsMinus1 ||
		sps.PicHeightInMapUnitsMinus1 != before.PicHeightInMapUnitsMinus1 {
		t.Fatalf("resolution changed: %dx%d -> %dx%d",
			before.PicWidthInMbsMinus1, before.PicHeightInMapUnitsMinus1,
			sps.PicWidthInMbsMinus1, sps.PicHeightInMapUnitsMinus1)
	}

	// Only the level_idc byte may differ.
	for i := range orig {
		if i == 3 {
			continue
		}
		if out[i] != orig[i] {
			t.Fatalf("byte %d changed unexpectedly: 0x%02x -> 0x%02x", i, orig[i], out[i])
		}
	}
}

// An SPS already at its minimal level must be returned untouched, sharing the
// caller's backing array (no allocation on the common path).
func TestClampSPSLevel_LeavesMinimalUnchanged(t *testing.T) {
	in := mustHex(t, garageSPSHex)
	out := clampSPSLevel(in)
	if &out[0] != &in[0] {
		t.Fatalf("already-minimal SPS was reallocated")
	}
	if out[3] != 0x16 {
		t.Fatalf("already-minimal level changed: 0x%02x", out[3])
	}
}

// A stream whose level is sufficient must never be raised.
func TestClampSPSLevel_NeverRaisesLevel(t *testing.T) {
	in := mustHex(t, garageSPSHex)
	out := clampSPSLevel(in)
	if out[3] > in[3] {
		t.Fatalf("level was raised: 0x%02x -> 0x%02x", in[3], out[3])
	}
}

// Garbage or truncated input must be passed through, never panic.
func TestClampSPSLevel_BadInputReturnsInput(t *testing.T) {
	for _, in := range [][]byte{nil, {}, {0x67}, {0x67, 0x64, 0x00}, {0x00, 0x01, 0x02, 0x03, 0x04}} {
		out := clampSPSLevel(in)
		if len(out) != len(in) {
			t.Fatalf("length changed for bad input %v -> %v", in, out)
		}
	}
}

// The level chosen must satisfy frame size, macroblock rate AND decode-buffer
// (reference-frame) limits, so it never under-declares a stream.
func TestMinH264Level(t *testing.T) {
	cases := []struct {
		name                    string
		frameMbs, fps, refFrame int
		want                    uint8
	}{
		// 640x480 at 10 fps, 1 ref: the front_door sub-stream -> level 2.2.
		{"640x480@10/1ref", 1200, 10, 1, 22},
		// Same frame at 30 fps needs more MB/s -> level 3.0.
		{"640x480@30/1ref", 1200, 30, 1, 30},
		// Many reference frames force a larger DPB even at a small resolution:
		// 16*1200=19200 mbs exceeds level 3.1's 18000, so level 3.2.
		{"640x480@10/16ref", 1200, 10, 16, 32},
		// 1080p60 with 4 refs: MB/s (489600) needs level 4.2, not 4.0.
		{"1080p60/4ref", 8160, 60, 4, 42},
		// 1080p30 with 4 refs fits level 4.0.
		{"1080p30/4ref", 8160, 30, 4, 40},
	}
	for _, c := range cases {
		if got := minH264Level(c.frameMbs, c.fps, c.refFrame); got != c.want {
			t.Errorf("%s: minH264Level=%d, want %d", c.name, got, c.want)
		}
	}
}

// The clamp must only fire when the frame rate is known; the production
// fixtures carry VUI timing, which is what makes front_door eligible.
func TestClampSPSLevel_RequiresKnownFrameRate(t *testing.T) {
	var fd h264.SPS
	if err := fd.Unmarshal(mustHex(t, frontDoorSPSHex)); err != nil {
		t.Fatalf("front_door SPS does not decode: %v", err)
	}
	if spsFrameRate(&fd) <= 0 {
		t.Fatal("front_door fixture must declare a frame rate for the clamp to apply")
	}
}

// Regression: HLS used the clamped SPS in avcC but left the camera's raw
// level-5.1 SPS inside every keyframe sample. VideoToolbox accepted the audio
// track and advanced playback while rendering the contradictory video black.
func TestReplaceAccessUnitSPSMatchesMuxInit(t *testing.T) {
	raw := mustHex(t, frontDoorSPSHex)
	mux := clampSPSLevel(raw)
	pps := []byte{0x68, 0xee, 0x3c, 0x80}
	idr := []byte{0x65, 0x88, 0x84}
	au := [][]byte{raw, pps, idr}

	got := replaceAccessUnitSPS(au, mux)
	if !bytes.Equal(got[0], mux) {
		t.Fatalf("keyframe SPS %x does not match mux SPS %x", got[0], mux)
	}
	if !bytes.Equal(au[0], raw) {
		t.Fatal("input access unit was mutated")
	}
	if !bytes.Equal(got[1], pps) || !bytes.Equal(got[2], idr) {
		t.Fatal("non-SPS NAL units changed")
	}
}

func TestReplaceAccessUnitSPSIsNoOpWhenAlreadyConsistent(t *testing.T) {
	mux := clampSPSLevel(mustHex(t, frontDoorSPSHex))
	au := [][]byte{mux, {0x65, 0x88, 0x84}}
	got := replaceAccessUnitSPS(au, mux)
	if &got[0] != &au[0] {
		t.Fatal("consistent access unit was unnecessarily copied")
	}
}
