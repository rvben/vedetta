package h264au

import "testing"

// NAL header byte for a given H264 NAL type (forbidden_zero=0, nri=0).
func nalHdr(t byte) byte { return t & 0x1F }

func auEqual(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if string(a[i]) != string(b[i]) {
			return false
		}
	}
	return true
}

// A camera (e.g. Tapo C220) injects a proprietary user-data SEI NAL that strict
// iOS VideoToolbox rejects as bad data (-8969), collapsing live HLS to a
// keyframe-only slideshow. DropSEI must remove every SEI (type 6) while
// preserving every other NAL in order.
func TestDropSEINALs_StripsSEIPreservesOrder(t *testing.T) {
	sps := []byte{nalHdr(7), 0x64, 0x00}
	pps := []byte{nalHdr(8), 0xea}
	sei := []byte{nalHdr(6), 0x05, 0x15, 'T', 'P', 'L', 'I', 'N', 'K'}
	idr := []byte{nalHdr(5), 0x88, 0x99}
	p := []byte{nalHdr(1), 0x41, 0x42}

	got := DropSEI([][]byte{sps, sei, pps, idr})
	if !auEqual(got, [][]byte{sps, pps, idr}) {
		t.Fatalf("SEI not stripped or order changed: %v", got)
	}

	// SEI appearing alongside a P-frame must still be dropped.
	got = DropSEI([][]byte{sei, p})
	if !auEqual(got, [][]byte{p}) {
		t.Fatalf("SEI before P-frame not stripped: %v", got)
	}
}

// When there is no SEI the input must be returned untouched (no allocation, no
// copy) so the common path stays cheap.
func TestDropSEINALs_NoSEIReturnsInput(t *testing.T) {
	sps := []byte{nalHdr(7)}
	idr := []byte{nalHdr(5)}
	in := [][]byte{sps, idr}
	out := DropSEI(in)
	if len(out) != 2 || &out[0] != &in[0] {
		t.Fatalf("no-SEI AU must be returned without reallocation")
	}
}

// An access unit that is only SEI collapses to empty so the caller can skip it
// rather than push an empty sample.
func TestDropSEINALs_SEIOnlyBecomesEmpty(t *testing.T) {
	sei := []byte{nalHdr(6), 0x05}
	if out := DropSEI([][]byte{sei}); len(out) != 0 {
		t.Fatalf("SEI-only AU must become empty, got %v", out)
	}
}

// Zero-length / nil NAL units must not panic (the type check guards indexing)
// and are left in place - stripping empties is not this function's job.
func TestDropSEINALs_HandlesEmptyNALs(t *testing.T) {
	sei := []byte{nalHdr(6), 0x05}
	idr := []byte{nalHdr(5), 0x01}
	got := DropSEI([][]byte{nil, {}, sei, idr})
	if len(got) != 3 || len(got[0]) != 0 || len(got[1]) != 0 || string(got[2]) != string(idr) {
		t.Fatalf("unexpected result with empty NALs: %v", got)
	}
}

// DropSEI must never mutate the caller's slice (it shares the RTP
// depacketizer's backing array with other consumers).
func TestDropSEINALs_DoesNotMutateInput(t *testing.T) {
	sps := []byte{nalHdr(7)}
	sei := []byte{nalHdr(6), 0x05}
	idr := []byte{nalHdr(5)}
	in := [][]byte{sps, sei, idr}
	_ = DropSEI(in)
	if len(in) != 3 || string(in[0]) != string(sps) || string(in[1]) != string(sei) || string(in[2]) != string(idr) {
		t.Fatalf("input slice was mutated: %v", in)
	}
}
