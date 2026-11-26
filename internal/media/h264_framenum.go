package media

import (
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
)

// withFrameNum returns the Annex B access unit with the frame_num of every
// slice set to n. The unit has to carry the SPS its slices use, which says how
// wide the field is. It reports false when the unit has no such SPS or a slice
// header cannot be read.
//
// A decoder refuses a picture whose frame_num does not follow on from the last
// one it decoded. Renumbering lets an intra picture be decoded straight after
// an IDR picture (frame_num 0), without the pictures the stream had in between.
func withFrameNum(unit []byte, n uint32) ([]byte, bool) {
	nalus := splitAnnexB(unit)
	var sps h264.SPS
	found := false
	for _, nalu := range nalus {
		if len(nalu) > 0 && h264.NALUType(nalu[0]&0x1F) == h264.NALUTypeSPS {
			if err := sps.Unmarshal(nalu); err != nil {
				return nil, false
			}
			found = true
			break
		}
	}
	if !found {
		return nil, false
	}
	width := int(sps.Log2MaxFrameNumMinus4) + 4
	if width > 16 || n >= 1<<width {
		return nil, false
	}

	out := make([]byte, 0, len(unit)+16)
	for _, nalu := range nalus {
		if len(nalu) == 0 {
			continue
		}
		switch h264.NALUType(nalu[0] & 0x1F) {
		case h264.NALUTypeIDR, h264.NALUTypeNonIDR:
			payload := h264.EmulationPreventionRemove(nalu)
			// first_mb_in_slice, slice_type and pic_parameter_set_id precede
			// frame_num, and colour_plane_id when the planes are coded apart.
			bit, ok := skipGolombs(payload, 8, 3)
			if !ok {
				return nil, false
			}
			if sps.SeparateColourPlaneFlag {
				bit += 2
			}
			if bit+width > len(payload)*8 {
				return nil, false
			}
			for i := range width {
				mask := byte(1) << (7 - (bit+i)%8)
				if n>>(width-1-i)&1 == 1 {
					payload[(bit+i)/8] |= mask
				} else {
					payload[(bit+i)/8] &^= mask
				}
			}
			out = append(out, 0, 0, 0, 1)
			out = appendEmulationPrevented(out, payload)
		default:
			out = append(out, 0, 0, 0, 1)
			out = append(out, nalu...)
		}
	}
	return out, true
}

// skipGolombs returns the bit position after n Exp-Golomb values that start at
// bit. It reports false when the data ends first.
func skipGolombs(data []byte, bit, n int) (int, bool) {
	end := len(data) * 8
	for range n {
		zeros := 0
		for {
			if bit >= end {
				return 0, false
			}
			one := data[bit/8]>>(7-bit%8)&1 == 1
			bit++
			if one {
				break
			}
			zeros++
		}
		bit += zeros
		if zeros > 32 || bit > end {
			return 0, false
		}
	}
	return bit, true
}

// appendEmulationPrevented appends a NAL unit to dst, inserting the emulation
// prevention bytes that keep its payload free of start codes: one ahead of any
// byte of 3 or less that follows two zero bytes, and one after two zero bytes
// that end the unit.
func appendEmulationPrevented(dst, nalu []byte) []byte {
	zeros := 0
	for _, b := range nalu {
		if zeros >= 2 && b <= 3 {
			dst = append(dst, 3)
			zeros = 0
		}
		dst = append(dst, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	if zeros >= 2 {
		dst = append(dst, 3)
	}
	return dst
}
