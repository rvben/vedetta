package media

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
)

// frameNums returns the frame_num of every slice in an Annex B unit whose SPS
// gives the field width bits.
func frameNums(t *testing.T, unit []byte, width int) []uint32 {
	t.Helper()
	var nums []uint32
	for _, nalu := range splitAnnexB(unit) {
		switch h264.NALUType(nalu[0] & 0x1F) {
		case h264.NALUTypeIDR, h264.NALUTypeNonIDR:
			payload := h264.EmulationPreventionRemove(nalu)
			bit, ok := skipGolombs(payload, 8, 3)
			if !ok {
				t.Fatalf("slice header of %x cannot be read", nalu[:min(len(nalu), 8)])
			}
			var n uint32
			for i := range width {
				n = n<<1 | uint32(payload[(bit+i)/8]>>(7-(bit+i)%8)&1)
			}
			nums = append(nums, n)
		}
	}
	return nums
}

// Renumbering changes frame_num and nothing else: putting the camera's number
// back gives the camera's bytes.
func TestWithFrameNumChangesOnlyTheFrameNumber(t *testing.T) {
	for _, fixture := range []string{"open_gop.h264", "open_gop_bframes.h264"} {
		raw, err := os.ReadFile(filepath.Join("testdata", fixture))
		if err != nil {
			t.Fatal(err)
		}
		// Access units that carry an SPS, each with a 4-byte start code per NAL unit.
		var units [][]byte
		for _, nalu := range splitAnnexB(raw) {
			if h264.NALUType(nalu[0]&0x1F) == h264.NALUTypeAccessUnitDelimiter {
				units = append(units, nil)
			}
			units[len(units)-1] = append(append(units[len(units)-1], 0, 0, 0, 1), nalu...)
		}
		renumbered := 0
		for _, unit := range units {
			if !bytes.Contains(unit, []byte{0, 0, 0, 1, 0x67}) {
				continue
			}
			var sps h264.SPS
			for _, nalu := range splitAnnexB(unit) {
				if h264.NALUType(nalu[0]&0x1F) == h264.NALUTypeSPS {
					if err := sps.Unmarshal(nalu); err != nil {
						t.Fatal(err)
					}
				}
			}
			width := int(sps.Log2MaxFrameNumMinus4) + 4
			original := frameNums(t, unit, width)
			if len(original) == 0 {
				t.Fatalf("%s: a keyframe without slices", fixture)
			}
			for _, n := range []uint32{0, 1, 1<<width - 1} {
				got, ok := withFrameNum(unit, n)
				if !ok {
					t.Fatalf("%s: frame_num %d could not be set", fixture, n)
				}
				for _, num := range frameNums(t, got, width) {
					if num != n {
						t.Fatalf("%s: a slice has frame_num %d, want %d", fixture, num, n)
					}
				}
				back, ok := withFrameNum(got, original[0])
				if !ok || !bytes.Equal(back, unit) {
					t.Fatalf("%s: setting frame_num to %d and back changed the unit", fixture, n)
				}
				if n != original[0] && bytes.Equal(got, unit) {
					t.Fatalf("%s: frame_num %d left the unit as it was", fixture, n)
				}
			}
			if _, ok := withFrameNum(unit, 1<<width); ok {
				t.Fatalf("%s: a frame_num of %d fits no %d-bit field", fixture, 1<<width, width)
			}
			renumbered++
		}
		if renumbered < 3 {
			t.Fatalf("%s: %d keyframes renumbered, want at least 3", fixture, renumbered)
		}
	}
}

func TestWithFrameNumRefusesAUnitItCannotRead(t *testing.T) {
	slice := []byte{0, 0, 0, 1, 0x41, 0x88, 0x84, 7}
	sps := append([]byte{0, 0, 0, 1}, openGOPSPS...)
	join := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

	if got, ok := withFrameNum(join(sps, slice), 1); !ok || !bytes.Equal(got, join(sps, []byte{0, 0, 0, 1, 0x41, 0x88, 0x8c, 7})) {
		t.Fatalf("got %x, %v, want the slice renumbered", got, ok)
	}
	// The emulation prevention bytes of a slice are put back where they were.
	escaped := []byte{0, 0, 0, 1, 0x41, 0x88, 0x84, 0, 0, 3, 1, 7, 0, 0, 3}
	want := join(sps, []byte{0, 0, 0, 1, 0x41, 0x88, 0x8c, 0, 0, 3, 1, 7, 0, 0, 3})
	if got, ok := withFrameNum(join(sps, escaped), 1); !ok || !bytes.Equal(got, want) {
		t.Fatalf("got %x, %v, want %x", got, ok, want)
	}
	cases := map[string][]byte{
		"no SPS":                 slice,
		"an SPS that is cut off": join([]byte{0, 0, 0, 1, 0x67, 0x42, 0, 30, 0x80}, slice),
		"a slice header cut off": join(sps, []byte{0, 0, 0, 1, 0x41, 0x88}),
		// first_mb_in_slice 0, slice_type 7, PPS 3 (00100), then three bits.
		"a slice ending in frame_num": join(sps, []byte{0, 0, 0, 1, 0x41, 0x88, 0x20}),
		"a slice with no payload":     join(sps, []byte{0, 0, 0, 1, 0x41}),
	}
	for name, unit := range cases {
		if got, ok := withFrameNum(unit, 1); ok {
			t.Errorf("%s: got %x, want the unit refused", name, got)
		}
	}
}

// bitsToBytes packs a string of 0s and 1s, most significant bit first, padded
// with zeros.
func bitsToBytes(fields ...string) []byte {
	var out []byte
	n := 0
	for _, field := range fields {
		for _, c := range field {
			if n%8 == 0 {
				out = append(out, 0)
			}
			if c == '1' {
				out[n/8] |= 1 << (7 - n%8)
			}
			n++
		}
	}
	return out
}

// With the colour planes coded apart, colour_plane_id sits ahead of frame_num.
func TestWithFrameNumSkipsTheColourPlaneID(t *testing.T) {
	// High 4:4:4 Predictive, level 3: SPS 0, chroma_format_idc 3 with
	// separate_colour_plane_flag, 8-bit, a frame_num of 4 bits, POC type 2,
	// one reference frame, 10x6 macroblocks, frames only.
	sps := append([]byte{0x67, 244, 0, 30}, bitsToBytes(
		"1", "00100", "1", "1", "1", "0", "0", "1", "011", "010", "0", "0001010", "00110", "1", "1", "0", "0", "1")...)
	var parsed h264.SPS
	if err := parsed.Unmarshal(sps); err != nil || !parsed.SeparateColourPlaneFlag || parsed.Log2MaxFrameNumMinus4 != 0 {
		t.Fatalf("SPS: err=%v separate=%v log2=%d, want separate planes and 4 bits", err, parsed.SeparateColourPlaneFlag, parsed.Log2MaxFrameNumMinus4)
	}
	// first_mb_in_slice 0, slice_type 7, PPS 0, colour_plane_id 2, frame_num, 111.
	slice := func(frameNum string) []byte {
		return append([]byte{0x41}, bitsToBytes("1", "0001000", "1", "10", frameNum, "111")...)
	}
	unit := func(frameNum string) []byte {
		return bytes.Join([][]byte{{0, 0, 0, 1}, sps, {0, 0, 0, 1}, slice(frameNum)}, nil)
	}
	if got, ok := withFrameNum(unit("0110"), 9); !ok || !bytes.Equal(got, unit("1001")) {
		t.Fatalf("got %x, %v, want %x", got, ok, unit("1001"))
	}
}

func TestSkipGolombs(t *testing.T) {
	cases := []struct {
		name   string
		data   []byte
		bit, n int
		want   int
		ok     bool
	}{
		{"three one-bit values", []byte{0xe0}, 0, 3, 3, true},
		{"a value of three bits and one of nine", []byte{0x41, 0x00}, 0, 2, 12, true},
		{"from an offset", []byte{0xff, 0x41, 0x00}, 8, 2, 20, true},
		{"none", []byte{0x00}, 3, 0, 3, true},
		{"a value that ends with the data", []byte{0x4b}, 0, 4, 8, true},
		{"the data ends in the prefix", []byte{0x00}, 0, 1, 0, false},
		{"the data ends in the suffix", []byte{0x01}, 0, 1, 0, false},
		{"starts past the end", []byte{0xff}, 8, 1, 0, false},
		{"empty", nil, 0, 1, 0, false},
		{"a prefix longer than any value", append(make([]byte, 5), 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff), 0, 1, 0, false},
	}
	for _, tc := range cases {
		if got, ok := skipGolombs(tc.data, tc.bit, tc.n); got != tc.want || ok != tc.ok {
			t.Errorf("%s: got %d, %v, want %d, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestAppendEmulationPrevented(t *testing.T) {
	cases := []struct {
		name     string
		in, want []byte
	}{
		{"nothing to escape", []byte{0x41, 0, 1, 0, 4}, []byte{0x41, 0, 1, 0, 4}},
		{"a start code", []byte{0x41, 0, 0, 1}, []byte{0x41, 0, 0, 3, 1}},
		{"each value up to three", []byte{0, 0, 0, 0, 0, 2, 0, 0, 3, 0, 0, 4}, []byte{0, 0, 3, 0, 0, 3, 0, 2, 0, 0, 3, 3, 0, 0, 4}},
		{"two final zeros", []byte{0x41, 0x80, 0, 0}, []byte{0x41, 0x80, 0, 0, 3}},
		{"four final zeros", []byte{0x41, 0, 0, 0, 0}, []byte{0x41, 0, 0, 3, 0, 0, 3}},
		{"one final zero", []byte{0x41, 0x80, 0}, []byte{0x41, 0x80, 0}},
		{"empty", nil, []byte{9}},
	}
	for _, tc := range cases {
		got := appendEmulationPrevented([]byte{9}, tc.in)
		want := tc.want
		if tc.in != nil {
			want = append([]byte{9}, tc.want...)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: got %x, want %x", tc.name, got, want)
		}
		if tc.in != nil && !bytes.Equal(h264.EmulationPreventionRemove(got[1:]), tc.in) {
			t.Errorf("%s: removing the prevention bytes gives %x, want %x", tc.name, h264.EmulationPreventionRemove(got[1:]), tc.in)
		}
	}
}
