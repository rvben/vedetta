package stream

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/rvben/vedetta/internal/rtsp"
)

// fakeWriter records every RTP packet passed to WriteRTP so tests can assert
// on the exact stream of packets that would reach a peer's TrackLocalStaticRTP.
type fakeWriter struct {
	mu      sync.Mutex
	packets []rtp.Packet
	err     error
}

func (f *fakeWriter) WriteRTP(pkt *rtp.Packet) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.packets = append(f.packets, *pkt)
	return nil
}

func (f *fakeWriter) snapshot() []rtp.Packet {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]rtp.Packet, len(f.packets))
	copy(out, f.packets)
	return out
}

// newTestPeer builds a peerState wired to a recording writer.
func newTestPeer(sps, pps []byte) (*peerState, *fakeWriter) {
	w := &fakeWriter{}
	return &peerState{
		video: &trackState{track: w},
		sps:   sps,
		pps:   pps,
	}, w
}

// idrPacket builds an RTP packet whose payload is a single bare IDR NAL.
func idrPacket(seq uint16, ts uint32) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           0xCAFEBABE,
			Marker:         true,
		},
		// NAL header: F=0, NRI=3 (IDR ref), Type=5 → 0x65, followed by slice data.
		Payload: []byte{0x65, 0x88, 0x84, 0x00, 0x00},
	}
}

// spsPacket builds an RTP packet whose payload is a single SPS NAL.
func spsPacket(seq uint16, ts uint32) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           0xCAFEBABE,
		},
		// NAL header type=7 → 0x67, then profile/constraints/level.
		Payload: []byte{0x67, 0x42, 0xe0, 0x1f, 0xab, 0xcd},
	}
}

// pPacket builds an RTP packet whose payload is a single P-slice NAL (type 1).
func pPacket(seq uint16, ts uint32) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           0xCAFEBABE,
		},
		Payload: []byte{0x41, 0x9a, 0x00},
	}
}

// stapAPacket builds a STAP-A packet aggregating the given NAL units.
func stapAPacket(seq uint16, ts uint32, nals ...[]byte) *rtp.Packet {
	payload := []byte{0x78} // STAP-A header: F=0, NRI=3, Type=24
	for _, nal := range nals {
		payload = append(payload, byte(len(nal)>>8), byte(len(nal)))
		payload = append(payload, nal...)
	}
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           0xCAFEBABE,
		},
		Payload: payload,
	}
}

// fuAPacket builds an FU-A fragment. nalType is the inner NAL type, startBit
// signals the first fragment of a NAL unit.
func fuAPacket(seq uint16, ts uint32, nalType byte, startBit bool) *rtp.Packet {
	fuIndicator := byte(0x7c) // F=0, NRI=3, Type=28 (FU-A)
	fuHeader := nalType & 0x1f
	if startBit {
		fuHeader |= 0x80
	}
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           0xCAFEBABE,
		},
		Payload: []byte{fuIndicator, fuHeader, 0x00, 0x01},
	}
}

func TestContainsParameterSets(t *testing.T) {
	tests := []struct {
		name string
		pkt  *rtp.Packet
		want bool
	}{
		{"single SPS", spsPacket(1, 0), true},
		{"single PPS", &rtp.Packet{Payload: []byte{0x68, 0xce, 0x3c, 0x80}}, true},
		{"single IDR", idrPacket(1, 0), false},
		{"single P-slice", pPacket(1, 0), false},
		{"STAP-A with SPS first", stapAPacket(1, 0, []byte{0x67, 0x42, 0xe0, 0x1f}, []byte{0x68, 0xce}), true},
		{"STAP-A with PPS only", stapAPacket(1, 0, []byte{0x68, 0xce, 0x3c, 0x80}), true},
		{"STAP-A with IDR first then SPS", stapAPacket(1, 0, []byte{0x65, 0x88, 0x84}, []byte{0x67, 0x42, 0xe0, 0x1f}), true},
		{"STAP-A without parameter sets", stapAPacket(1, 0, []byte{0x41, 0x9a, 0x00}, []byte{0x41, 0x9b, 0x01}), false},
		{"FU-A start of IDR", fuAPacket(1, 0, 5, true), false},
		{"empty payload", &rtp.Packet{Payload: nil}, false},
		{"truncated STAP-A header", &rtp.Packet{Payload: []byte{0x78, 0x00}}, false},
		{"STAP-A oversized size", &rtp.Packet{Payload: []byte{0x78, 0x00, 0xff, 0x67}}, false},
		{"STAP-A zero-sized NAL", &rtp.Packet{Payload: []byte{0x78, 0x00, 0x00, 0x67, 0x42}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := containsParameterSets(tc.pkt); got != tc.want {
				t.Errorf("containsParameterSets() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildNALPacketCopiesHeaderAndPayload(t *testing.T) {
	template := idrPacket(1000, 90000)
	nal := []byte{0x67, 0x42, 0xe0, 0x1f}

	out := buildNALPacket(template, nal)

	if out.PayloadType != template.PayloadType {
		t.Errorf("PayloadType = %d, want %d", out.PayloadType, template.PayloadType)
	}
	if out.SSRC != template.SSRC {
		t.Errorf("SSRC = %x, want %x", out.SSRC, template.SSRC)
	}
	if out.Timestamp != template.Timestamp {
		t.Errorf("Timestamp = %d, want %d", out.Timestamp, template.Timestamp)
	}
	if out.Marker {
		t.Error("synthetic parameter-set packet should not have marker bit set")
	}
	if string(out.Payload) != string(nal) {
		t.Errorf("Payload = %x, want %x", out.Payload, nal)
	}
	// Mutating the input NAL after construction must not affect the packet.
	nal[0] = 0xFF
	if out.Payload[0] == 0xFF {
		t.Error("Payload aliases caller's slice — buildNALPacket should copy")
	}
}

// makeBigNAL builds a payload that starts with a single-NAL header of the
// given type and pads the rest with deterministic, non-zero data of length
// `size-1`. The total payload length is `size`.
func makeBigNAL(nalType byte, size int) []byte {
	out := make([]byte, size)
	out[0] = 0x60 | (nalType & 0x1f) // F=0, NRI=3, Type=nalType
	for i := 1; i < size; i++ {
		out[i] = byte(i)
	}
	return out
}

func TestFragmentSingleNALSkipsSmallPackets(t *testing.T) {
	pkt := idrPacket(1000, 90000)
	if got := fragmentSingleNAL(pkt, fuaMTU); got != nil {
		t.Errorf("small packet fragmented unexpectedly: %d fragments", len(got))
	}
}

func TestFragmentSingleNALPassthroughForFUAndSTAP(t *testing.T) {
	// FU-A and STAP-A must be left intact even if they are larger than MTU —
	// fragmenting framing types would corrupt them.
	cases := []*rtp.Packet{
		fuAPacket(1, 0, 5, true),
		stapAPacket(1, 0, make([]byte, 4000), make([]byte, 4000)),
	}
	for i, pkt := range cases {
		if got := fragmentSingleNAL(pkt, fuaMTU); got != nil {
			t.Errorf("case %d: framing packet was fragmented (%d outputs)", i, len(got))
		}
	}
}

func TestFragmentSingleNALProducesValidFUA(t *testing.T) {
	// Build a 5000-byte IDR. With mtu=1200 we expect ceil((5000-1)/1198) = 5
	// fragments. NAL data is 4999 bytes; each fragment carries up to 1198
	// bytes after the 2-byte FU header.
	nal := makeBigNAL(5, 5000)
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version: 2, PayloadType: 96, SequenceNumber: 1000,
			Timestamp: 90000, SSRC: 0xCAFEBABE, Marker: true,
		},
		Payload: nal,
	}

	frags := fragmentSingleNAL(pkt, 1200)
	if frags == nil {
		t.Fatal("expected fragments, got nil")
	}
	const fragSize = 1200 - 2
	wantCount := (len(nal) - 1 + fragSize - 1) / fragSize
	if len(frags) != wantCount {
		t.Fatalf("got %d fragments, want %d", len(frags), wantCount)
	}

	// Reassemble: NAL header byte should be reconstructable from FU indicator
	// (top 3 bits NRI/F) + FU header low 5 bits.
	reassembled := []byte{(frags[0].Payload[0] & 0xe0) | (frags[0].Payload[1] & 0x1f)}
	for i, f := range frags {
		if f.PayloadType != pkt.PayloadType {
			t.Errorf("fragment %d PT = %d, want %d", i, f.PayloadType, pkt.PayloadType)
		}
		if f.SSRC != pkt.SSRC {
			t.Errorf("fragment %d SSRC = %x, want %x", i, f.SSRC, pkt.SSRC)
		}
		if f.Timestamp != pkt.Timestamp {
			t.Errorf("fragment %d ts = %d, want %d", i, f.Timestamp, pkt.Timestamp)
		}
		if got := f.Payload[0] & 0x1f; got != 28 {
			t.Errorf("fragment %d outer NAL type = %d, want 28 (FU-A)", i, got)
		}
		if got := f.Payload[1] & 0x1f; got != 5 {
			t.Errorf("fragment %d inner NAL type = %d, want 5 (IDR)", i, got)
		}
		startBit := f.Payload[1]&0x80 != 0
		endBit := f.Payload[1]&0x40 != 0
		isFirst := i == 0
		isLast := i == len(frags)-1
		if startBit != isFirst {
			t.Errorf("fragment %d start bit = %v, want %v", i, startBit, isFirst)
		}
		if endBit != isLast {
			t.Errorf("fragment %d end bit = %v, want %v", i, endBit, isLast)
		}
		if f.Marker != (isLast && pkt.Marker) {
			t.Errorf("fragment %d marker = %v, want %v", i, f.Marker, isLast && pkt.Marker)
		}
		if len(f.Payload) > 1200 {
			t.Errorf("fragment %d exceeds MTU (%d bytes)", i, len(f.Payload))
		}
		reassembled = append(reassembled, f.Payload[2:]...)
	}
	if string(reassembled) != string(nal) {
		t.Errorf("reassembled NAL differs from original (len %d vs %d)", len(reassembled), len(nal))
	}
}

// bigFUAPacket builds an FU-A packet of the requested total payload size with
// the given NRI, inner NAL type, and start/end bits. The NAL-data portion is
// filled with a deterministic non-zero pattern so reassembly can be verified
// byte-for-byte.
func bigFUAPacket(seq uint16, ts uint32, nri, nalType byte, startBit, endBit, marker bool, size int) *rtp.Packet {
	if size < 2 {
		panic("bigFUAPacket size must be >= 2")
	}
	fuIndicator := (nri & 0x60) | 28 // type 28 = FU-A
	fuHeader := nalType & 0x1f
	if startBit {
		fuHeader |= 0x80
	}
	if endBit {
		fuHeader |= 0x40
	}
	payload := make([]byte, size)
	payload[0] = fuIndicator
	payload[1] = fuHeader
	for i := 2; i < size; i++ {
		payload[i] = byte(i)
	}
	return &rtp.Packet{
		Header: rtp.Header{
			Version: 2, PayloadType: 96, SequenceNumber: seq,
			Timestamp: ts, SSRC: 0xCAFEBABE, Marker: marker,
		},
		Payload: payload,
	}
}

func TestRefragmentFUASkipsSmallPackets(t *testing.T) {
	pkt := bigFUAPacket(1, 0, 0x60, 5, true, false, false, 1200)
	if got := refragmentFUA(pkt, 1200); got != nil {
		t.Errorf("FU-A within MTU was re-fragmented (%d outputs)", len(got))
	}
}

func TestRefragmentFUARejectsNonFUA(t *testing.T) {
	cases := map[string]*rtp.Packet{
		"single IDR":  {Payload: append([]byte{0x65}, make([]byte, 5000)...)},
		"STAP-A":      stapAPacket(1, 0, make([]byte, 4000), make([]byte, 4000)),
		"empty":       {Payload: nil},
		"single-byte": {Payload: []byte{0x7c}},
	}
	for name, pkt := range cases {
		if got := refragmentFUA(pkt, 1200); got != nil {
			t.Errorf("%s: non-FU-A was fragmented (%d outputs)", name, len(got))
		}
	}
}

func TestRefragmentFUAPreservesStartBitOnlyOnFirstPiece(t *testing.T) {
	// Head FU-A of an IDR (S=1, E=0). Splitting it must keep S=1 on the
	// first piece and S=0 on all subsequent pieces; no piece gets E=1.
	pkt := bigFUAPacket(1, 90000, 0x60, 5, true, false, false, 5000)
	frags := refragmentFUA(pkt, 1200)
	if len(frags) < 2 {
		t.Fatalf("expected ≥2 fragments, got %d", len(frags))
	}
	for i, f := range frags {
		gotStart := f.Payload[1]&0x80 != 0
		gotEnd := f.Payload[1]&0x40 != 0
		wantStart := i == 0
		if gotStart != wantStart {
			t.Errorf("fragment %d start bit = %v, want %v", i, gotStart, wantStart)
		}
		if gotEnd {
			t.Errorf("fragment %d unexpectedly has end bit set", i)
		}
		if f.Marker {
			t.Errorf("fragment %d unexpectedly has marker bit set", i)
		}
	}
}

func TestRefragmentFUAPreservesEndBitOnlyOnLastPiece(t *testing.T) {
	// Tail FU-A of an IDR (S=0, E=1, marker=1). Splitting it must keep E=1
	// and marker on the last piece only; no piece gets S=1.
	pkt := bigFUAPacket(1, 90000, 0x60, 5, false, true, true, 5000)
	frags := refragmentFUA(pkt, 1200)
	if len(frags) < 2 {
		t.Fatalf("expected ≥2 fragments, got %d", len(frags))
	}
	last := len(frags) - 1
	for i, f := range frags {
		gotStart := f.Payload[1]&0x80 != 0
		gotEnd := f.Payload[1]&0x40 != 0
		wantEnd := i == last
		if gotStart {
			t.Errorf("fragment %d unexpectedly has start bit set", i)
		}
		if gotEnd != wantEnd {
			t.Errorf("fragment %d end bit = %v, want %v", i, gotEnd, wantEnd)
		}
		if f.Marker != wantEnd {
			t.Errorf("fragment %d marker = %v, want %v", i, f.Marker, wantEnd)
		}
	}
}

func TestRefragmentFUAMiddleFragmentHasNoStartOrEnd(t *testing.T) {
	// Middle FU-A (S=0, E=0). No emitted piece may set S or E.
	pkt := bigFUAPacket(1, 90000, 0x60, 5, false, false, false, 5000)
	frags := refragmentFUA(pkt, 1200)
	if len(frags) < 2 {
		t.Fatalf("expected ≥2 fragments, got %d", len(frags))
	}
	for i, f := range frags {
		if f.Payload[1]&0x80 != 0 {
			t.Errorf("middle-fragment piece %d unexpectedly has start bit", i)
		}
		if f.Payload[1]&0x40 != 0 {
			t.Errorf("middle-fragment piece %d unexpectedly has end bit", i)
		}
	}
}

func TestRefragmentFUASingleSAndEFragmentSplitsCorrectly(t *testing.T) {
	// Rare case: a complete NAL delivered as a single FU-A with both S and
	// E set. After re-fragmentation only the first piece keeps S and only
	// the last piece keeps E (and the marker).
	pkt := bigFUAPacket(1, 90000, 0x60, 5, true, true, true, 5000)
	frags := refragmentFUA(pkt, 1200)
	if len(frags) < 2 {
		t.Fatalf("expected ≥2 fragments, got %d", len(frags))
	}
	last := len(frags) - 1
	for i, f := range frags {
		gotStart := f.Payload[1]&0x80 != 0
		gotEnd := f.Payload[1]&0x40 != 0
		if gotStart != (i == 0) {
			t.Errorf("piece %d start bit = %v, want %v", i, gotStart, i == 0)
		}
		if gotEnd != (i == last) {
			t.Errorf("piece %d end bit = %v, want %v", i, gotEnd, i == last)
		}
		if f.Marker != (i == last) {
			t.Errorf("piece %d marker = %v, want %v", i, f.Marker, i == last)
		}
	}
}

func TestRefragmentFUAReassemblesToOriginalNAL(t *testing.T) {
	// Concatenating the NAL-data slices from each re-fragmented piece must
	// reproduce the original FU-A's NAL data byte-for-byte. This is the
	// receiver-side invariant: re-fragmentation is lossless.
	pkt := bigFUAPacket(1, 90000, 0x60, 5, true, false, false, 5000)
	originalNALData := append([]byte(nil), pkt.Payload[2:]...)

	frags := refragmentFUA(pkt, 1200)
	if frags == nil {
		t.Fatal("expected fragments, got nil")
	}
	var reassembled []byte
	for _, f := range frags {
		if len(f.Payload) > 1200 {
			t.Errorf("piece exceeds MTU: %d bytes", len(f.Payload))
		}
		reassembled = append(reassembled, f.Payload[2:]...)
	}
	if string(reassembled) != string(originalNALData) {
		t.Errorf("reassembled NAL data (%d bytes) differs from original (%d bytes)",
			len(reassembled), len(originalNALData))
	}
}

func TestRefragmentFUAPreservesHeaderFields(t *testing.T) {
	// Each emitted piece must carry the same PT/SSRC/Timestamp as the
	// original FU-A and preserve the FU indicator's NRI and type bits.
	pkt := bigFUAPacket(42, 12345, 0x60, 5, true, false, false, 5000)
	wantIndicator := pkt.Payload[0]
	frags := refragmentFUA(pkt, 1200)
	if frags == nil {
		t.Fatal("expected fragments, got nil")
	}
	for i, f := range frags {
		if f.PayloadType != pkt.PayloadType {
			t.Errorf("piece %d PT = %d, want %d", i, f.PayloadType, pkt.PayloadType)
		}
		if f.SSRC != pkt.SSRC {
			t.Errorf("piece %d SSRC = %x, want %x", i, f.SSRC, pkt.SSRC)
		}
		if f.Timestamp != pkt.Timestamp {
			t.Errorf("piece %d ts = %d, want %d", i, f.Timestamp, pkt.Timestamp)
		}
		if f.Payload[0] != wantIndicator {
			t.Errorf("piece %d FU indicator = %02x, want %02x", i, f.Payload[0], wantIndicator)
		}
		if got := f.Payload[1] & 0x1f; got != 5 {
			t.Errorf("piece %d inner NAL type = %d, want 5", i, got)
		}
	}
}

func TestWriteVideoRefragmentsOversizedFUAIDR(t *testing.T) {
	// End-to-end: a 10 KB FU-A start fragment of an IDR must be re-fragmented
	// into multiple ≤1200-byte FU-A pieces and forwarded. This is the actual
	// Tapo C200 sub-stream behavior the fix addresses.
	peer, w := newTestPeer(nil, nil) // no SPS/PPS — start of FU-A IDR is its own keyframe trigger
	big := bigFUAPacket(1, 90000, 0x60, 5, true, false, false, 10000)
	if err := peer.writeVideo(big); err != nil {
		t.Fatalf("writeVideo: %v", err)
	}
	pkts := w.snapshot()
	if len(pkts) < 2 {
		t.Fatalf("expected multiple FU-A pieces, got %d", len(pkts))
	}
	for i, p := range pkts {
		if len(p.Payload) > fuaMTU {
			t.Errorf("piece %d exceeds MTU: %d bytes", i, len(p.Payload))
		}
		if got := p.Payload[0] & 0x1f; got != 28 {
			t.Errorf("piece %d outer type = %d, want 28 (FU-A)", i, got)
		}
		if got := p.Payload[1] & 0x1f; got != 5 {
			t.Errorf("piece %d inner type = %d, want 5 (IDR)", i, got)
		}
	}
	// Only the first piece should carry the start bit (the original was a
	// head FU-A); no piece should carry the end bit.
	for i, p := range pkts {
		startBit := p.Payload[1]&0x80 != 0
		if startBit != (i == 0) {
			t.Errorf("piece %d start bit = %v, want %v", i, startBit, i == 0)
		}
		if p.Payload[1]&0x40 != 0 {
			t.Errorf("piece %d unexpectedly has end bit set", i)
		}
	}
}

func TestWriteVideoFragmentsLargeIDR(t *testing.T) {
	peer, w := newTestPeer([]byte{0x67, 0x42}, []byte{0x68, 0xce})
	// 4500-byte IDR forces at least 4 FU-A fragments at mtu=1200.
	big := &rtp.Packet{
		Header: rtp.Header{
			Version: 2, PayloadType: 96, SequenceNumber: 1000,
			Timestamp: 90000, SSRC: 0xCAFEBABE, Marker: true,
		},
		Payload: makeBigNAL(5, 4500),
	}
	if err := peer.writeVideo(big); err != nil {
		t.Fatalf("writeVideo: %v", err)
	}
	pkts := w.snapshot()
	if len(pkts) < 6 { // SPS, PPS, plus ≥4 fragments
		t.Fatalf("expected ≥6 packets (SPS, PPS, fragments), got %d", len(pkts))
	}
	// First two are SPS/PPS, then every following packet should be FU-A.
	if got := pkts[0].Payload[0] & 0x1f; got != 7 {
		t.Errorf("first packet NAL type = %d, want 7 (SPS)", got)
	}
	if got := pkts[1].Payload[0] & 0x1f; got != 8 {
		t.Errorf("second packet NAL type = %d, want 8 (PPS)", got)
	}
	for i := 2; i < len(pkts); i++ {
		if got := pkts[i].Payload[0] & 0x1f; got != 28 {
			t.Errorf("packet %d NAL type = %d, want 28 (FU-A)", i, got)
		}
		if len(pkts[i].Payload) > fuaMTU {
			t.Errorf("packet %d size = %d, exceeds fuaMTU=%d", i, len(pkts[i].Payload), fuaMTU)
		}
		// All packets carry the same timestamp (single access unit).
		if pkts[i].Timestamp != pkts[2].Timestamp {
			t.Errorf("packet %d ts = %d, want %d", i, pkts[i].Timestamp, pkts[2].Timestamp)
		}
		// Sequence numbers must be strictly monotonic.
		if i > 0 && pkts[i].SequenceNumber != pkts[i-1].SequenceNumber+1 {
			t.Errorf("packet %d seq = %d, want %d (monotonic)", i, pkts[i].SequenceNumber, pkts[i-1].SequenceNumber+1)
		}
	}
	// Only the very last fragment should have the marker bit set.
	for i := 0; i < len(pkts)-1; i++ {
		if pkts[i].Marker {
			t.Errorf("non-final packet %d has marker bit set", i)
		}
	}
	if !pkts[len(pkts)-1].Marker {
		t.Error("final FU-A fragment is missing marker bit")
	}
}

func TestWriteVideoClearsMarkerOnSPSAndPPS(t *testing.T) {
	// Some cameras (Tapo C200) ship SPS, PPS and IDR as three RTP packets
	// sharing one access-unit timestamp but all wrongly carrying marker=1.
	// Chrome's H.264 depacketizer reads marker=1 as the end of an access
	// unit, so it produces three "frames" of which only the third has a
	// slice and the decoder never produces a picture. We must clear the
	// marker bit on SPS/PPS so Chrome assembles a single access unit.
	peer, w := newTestPeer(nil, nil)

	sps := spsPacket(100, 12345)
	sps.Marker = true
	pps := &rtp.Packet{
		Header: rtp.Header{
			Version: 2, PayloadType: 96, SequenceNumber: 101,
			Timestamp: 12345, SSRC: 0xCAFEBABE, Marker: true,
		},
		Payload: []byte{0x68, 0xce, 0x3c, 0x80},
	}
	idr := idrPacket(102, 12345) // helper already sets Marker=true

	for _, pk := range []*rtp.Packet{sps, pps, idr} {
		if err := peer.writeVideo(pk); err != nil {
			t.Fatalf("writeVideo: %v", err)
		}
	}

	pkts := w.snapshot()
	if len(pkts) != 3 {
		t.Fatalf("expected 3 forwarded packets, got %d", len(pkts))
	}
	if pkts[0].Marker {
		t.Errorf("SPS forwarded with marker=true, want false")
	}
	if pkts[1].Marker {
		t.Errorf("PPS forwarded with marker=true, want false")
	}
	if !pkts[2].Marker {
		t.Errorf("IDR forwarded with marker=false, want true (slice ends the AU)")
	}
}

func TestWriteVideoMonotonicSequence(t *testing.T) {
	peer, w := newTestPeer(nil, nil)

	// IDR followed by P-frames; sequence numbers must be 0, 1, 2 regardless
	// of the inbound sequence numbers.
	pkts := []*rtp.Packet{
		idrPacket(40000, 90000),
		pPacket(40001, 93000),
		pPacket(40002, 96000),
	}
	for _, pk := range pkts {
		if err := peer.writeVideo(pk); err != nil {
			t.Fatal(err)
		}
	}
	got := w.snapshot()
	if len(got) != 3 {
		t.Fatalf("got %d packets, want 3", len(got))
	}
	for i, p := range got {
		if p.SequenceNumber != uint16(i) {
			t.Errorf("packet %d seq = %d, want %d", i, p.SequenceNumber, i)
		}
	}
}

func TestWriteVideoDropsPacketsUntilKeyframe(t *testing.T) {
	peer, w := newTestPeer(nil, nil)

	// Pre-keyframe P-slices must be dropped silently.
	for i := uint16(0); i < 5; i++ {
		if err := peer.writeVideo(pPacket(i, uint32(i)*3000)); err != nil {
			t.Fatalf("writeVideo P-slice %d: %v", i, err)
		}
	}
	if got := len(w.snapshot()); got != 0 {
		t.Fatalf("expected 0 packets forwarded before keyframe, got %d", got)
	}
}

func TestWriteVideoInjectsParameterSetsBeforeBareIDR(t *testing.T) {
	sps := []byte{0x67, 0x42, 0xe0, 0x1f, 0xab, 0xcd}
	pps := []byte{0x68, 0xce, 0x3c, 0x80}
	peer, w := newTestPeer(sps, pps)

	// Bare IDR arrives first; the peer must synthesize SPS+PPS in front of it.
	idr := idrPacket(1000, 90000)
	if err := peer.writeVideo(idr); err != nil {
		t.Fatalf("writeVideo: %v", err)
	}

	pkts := w.snapshot()
	if len(pkts) != 3 {
		t.Fatalf("expected 3 packets (SPS, PPS, IDR), got %d", len(pkts))
	}

	// trackState rewrites the first packet's seq/ts to 0; subsequent packets
	// are offset relative to that — so the stream the peer sees is 0, 1, 2 at ts=0.
	for i, want := range []struct {
		seq uint16
		ts  uint32
		nal byte
	}{
		{0, 0, 7}, // SPS
		{1, 0, 8}, // PPS
		{2, 0, 5}, // IDR
	} {
		if pkts[i].SequenceNumber != want.seq {
			t.Errorf("packet %d seq = %d, want %d", i, pkts[i].SequenceNumber, want.seq)
		}
		if pkts[i].Timestamp != want.ts {
			t.Errorf("packet %d ts = %d, want %d", i, pkts[i].Timestamp, want.ts)
		}
		if len(pkts[i].Payload) < 1 {
			t.Fatalf("packet %d empty payload", i)
		}
		if got := pkts[i].Payload[0] & 0x1f; got != want.nal {
			t.Errorf("packet %d NAL type = %d, want %d", i, got, want.nal)
		}
		if pkts[i].SSRC != idr.SSRC {
			t.Errorf("packet %d SSRC = %x, want %x", i, pkts[i].SSRC, idr.SSRC)
		}
	}
	// SPS and PPS payloads must round-trip verbatim.
	if string(pkts[0].Payload) != string(sps) {
		t.Errorf("SPS payload mismatch: got %x, want %x", pkts[0].Payload, sps)
	}
	if string(pkts[1].Payload) != string(pps) {
		t.Errorf("PPS payload mismatch: got %x, want %x", pkts[1].Payload, pps)
	}
}

func TestWriteVideoForwardsSubsequentPacketsAfterInjection(t *testing.T) {
	peer, w := newTestPeer([]byte{0x67, 0x42}, []byte{0x68, 0xce})

	if err := peer.writeVideo(idrPacket(1000, 90000)); err != nil {
		t.Fatalf("writeVideo IDR: %v", err)
	}
	// Subsequent P-frames must flow through without further injection.
	for i := uint16(1); i <= 3; i++ {
		if err := peer.writeVideo(pPacket(1000+i, 90000+uint32(i)*3000)); err != nil {
			t.Fatalf("writeVideo P-frame %d: %v", i, err)
		}
	}
	pkts := w.snapshot()
	if len(pkts) != 6 { // SPS, PPS, IDR, P1, P2, P3
		t.Fatalf("expected 6 packets total, got %d", len(pkts))
	}
	// P-slices should NOT be preceded by SPS/PPS.
	for i := 3; i < 6; i++ {
		if got := pkts[i].Payload[0] & 0x1f; got != 1 {
			t.Errorf("packet %d NAL type = %d, want 1 (P-slice)", i, got)
		}
	}
}

func TestWriteVideoSkipsInjectionWhenPacketAlreadyHasSPS(t *testing.T) {
	peer, w := newTestPeer([]byte{0x67, 0x42}, []byte{0x68, 0xce})

	// First packet IS the SPS — no injection needed.
	if err := peer.writeVideo(spsPacket(1000, 90000)); err != nil {
		t.Fatalf("writeVideo SPS: %v", err)
	}
	pkts := w.snapshot()
	if len(pkts) != 1 {
		t.Fatalf("expected 1 packet (no injection), got %d", len(pkts))
	}
	if got := pkts[0].Payload[0] & 0x1f; got != 7 {
		t.Errorf("first NAL type = %d, want 7 (SPS)", got)
	}
}

func TestWriteVideoSkipsInjectionForSTAPAWithParameterSets(t *testing.T) {
	peer, w := newTestPeer([]byte{0x67, 0x42}, []byte{0x68, 0xce})

	stap := stapAPacket(1000, 90000,
		[]byte{0x67, 0x42, 0xe0, 0x1f}, // SPS
		[]byte{0x68, 0xce, 0x3c, 0x80}, // PPS
		[]byte{0x65, 0x88, 0x84},       // IDR
	)
	if err := peer.writeVideo(stap); err != nil {
		t.Fatalf("writeVideo: %v", err)
	}
	if got := len(w.snapshot()); got != 1 {
		t.Errorf("expected 1 packet (no injection), got %d", got)
	}
}

func TestWriteVideoInjectsForFUAStartOfIDR(t *testing.T) {
	peer, w := newTestPeer([]byte{0x67, 0x42}, []byte{0x68, 0xce})

	// FU-A start fragment of an IDR — the FU-A is the keyframe trigger but
	// it carries no SPS/PPS, so injection is required.
	fu := fuAPacket(1000, 90000, 5, true)
	if err := peer.writeVideo(fu); err != nil {
		t.Fatalf("writeVideo: %v", err)
	}
	pkts := w.snapshot()
	if len(pkts) != 3 {
		t.Fatalf("expected 3 packets (SPS, PPS, FU-A), got %d", len(pkts))
	}
	wantNALs := []byte{7, 8, 28}
	for i, want := range wantNALs {
		if got := pkts[i].Payload[0] & 0x1f; got != want {
			t.Errorf("packet %d NAL type = %d, want %d", i, got, want)
		}
	}
}

func TestWriteVideoNoInjectionWhenParameterSetsMissing(t *testing.T) {
	// peerState with no cached SPS/PPS — fallback gracefully.
	peer, w := newTestPeer(nil, nil)
	if err := peer.writeVideo(idrPacket(1000, 90000)); err != nil {
		t.Fatalf("writeVideo: %v", err)
	}
	pkts := w.snapshot()
	if len(pkts) != 1 {
		t.Fatalf("expected 1 packet (IDR only), got %d", len(pkts))
	}
	if got := pkts[0].Payload[0] & 0x1f; got != 5 {
		t.Errorf("NAL type = %d, want 5 (IDR)", got)
	}
}

func TestWriteVideoOnlyOneInjectionPerSession(t *testing.T) {
	peer, w := newTestPeer([]byte{0x67, 0x42}, []byte{0x68, 0xce})

	// Two IDRs back-to-back: only the first triggers injection (keyframeSeen
	// flips on the first); the second is forwarded as-is. The browser will
	// see the SPS/PPS once at session start, which is sufficient.
	if err := peer.writeVideo(idrPacket(1000, 90000)); err != nil {
		t.Fatal(err)
	}
	if err := peer.writeVideo(idrPacket(1001, 93000)); err != nil {
		t.Fatal(err)
	}

	pkts := w.snapshot()
	if len(pkts) != 4 { // SPS, PPS, IDR1, IDR2
		t.Fatalf("expected 4 packets, got %d", len(pkts))
	}
	wantNALs := []byte{7, 8, 5, 5}
	for i, want := range wantNALs {
		if got := pkts[i].Payload[0] & 0x1f; got != want {
			t.Errorf("packet %d NAL type = %d, want %d", i, got, want)
		}
	}
}

func TestWriteVideoPropagatesWriteError(t *testing.T) {
	peer, w := newTestPeer([]byte{0x67, 0x42}, []byte{0x68, 0xce})
	wantErr := errors.New("peer closed")
	w.err = wantErr

	if err := peer.writeVideo(idrPacket(1000, 90000)); !errors.Is(err, wantErr) {
		t.Errorf("expected %v, got %v", wantErr, err)
	}
}

func TestWriteAudioBlockedUntilKeyframe(t *testing.T) {
	audioW := &fakeWriter{}
	peer := &peerState{
		audio: &trackState{track: audioW},
		video: &trackState{track: &fakeWriter{}},
	}

	// Audio arriving before any keyframe must be dropped to keep the browser's
	// audio decoder from starting before the video decoder has SPS/PPS.
	if err := peer.writeAudio(&rtp.Packet{Payload: []byte{0xFF}}); err != nil {
		t.Fatal(err)
	}
	if got := len(audioW.snapshot()); got != 0 {
		t.Errorf("audio forwarded before keyframe (got %d packets)", got)
	}

	// After the first keyframe, audio flows.
	peer.keyframeSeen = true
	if err := peer.writeAudio(&rtp.Packet{Payload: []byte{0xFF}}); err != nil {
		t.Fatal(err)
	}
	if got := len(audioW.snapshot()); got != 1 {
		t.Errorf("expected 1 audio packet after keyframe, got %d", got)
	}
}

func TestSDPOfferAnswerExchange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hub := rtsp.NewHub(ctx)
	defer hub.Close()

	sm := NewStreamManager(hub, nil)
	defer sm.Close()

	// Create a client peer connection to generate an offer
	clientConfig := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}

	client, err := webrtc.NewPeerConnection(clientConfig)
	if err != nil {
		t.Fatalf("failed to create client peer connection: %v", err)
	}
	defer func() { _ = client.Close() }()

	// Add a transceiver to receive video
	if _, err := client.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		t.Fatalf("failed to add transceiver: %v", err)
	}

	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatalf("failed to create offer: %v", err)
	}

	if err := client.SetLocalDescription(offer); err != nil {
		t.Fatalf("failed to set local description: %v", err)
	}

	// HandleOffer should succeed for the SDP exchange part,
	// even though the RTSP source won't have actual video.
	answer, err := sm.HandleOffer("test-cam", "rtsp://invalid:554/stream", offer)
	if err != nil {
		t.Logf("HandleOffer returned error (expected, no stream): %v", err)
		return
	}
	if answer.Type != webrtc.SDPTypeAnswer {
		t.Errorf("expected SDP answer type, got %v", answer.Type)
	}

	// The answer must advertise the rtcp feedback Chrome's H264 receiver
	// expects. Without these lines the decoder pipeline silently fails to
	// commit and framesDecoded stays at 0 even though packets are received
	// and counted as keyframes in inbound-rtp stats.
	//
	// Pion negotiates a single payload type from the offer (often 102, not
	// our registered 96), so locate the H264 PT in the answer and assert
	// against that.
	pt := extractH264PayloadType(t, answer.SDP)
	for _, fb := range []string{
		"goog-remb",
		"ccm fir",
		"nack",
		"nack pli",
		"transport-cc",
	} {
		line := fmt.Sprintf("a=rtcp-fb:%s %s", pt, fb)
		if !strings.Contains(answer.SDP, line) {
			t.Errorf("answer SDP missing %q:\n%s", line, answer.SDP)
		}
	}
}

// extractH264PayloadType returns the payload type assigned to H264/90000 in the
// rtpmap line of sdp. Fails the test if no H264 rtpmap is found.
func extractH264PayloadType(t *testing.T, sdp string) string {
	t.Helper()
	re := regexp.MustCompile(`a=rtpmap:(\d+) H264/90000`)
	m := re.FindStringSubmatch(sdp)
	if len(m) != 2 {
		t.Fatalf("no H264 rtpmap line found in answer SDP:\n%s", sdp)
	}
	return m[1]
}

func TestNewStreamManager(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hub := rtsp.NewHub(ctx)
	defer hub.Close()

	sm := NewStreamManager(hub, nil)
	if sm == nil {
		t.Fatal("NewStreamManager returned nil")
	}
	sm.Close()
}

func TestStreamManagerReattachesAfterSourceReplacement(t *testing.T) {
	hub := rtsp.NewHub(context.Background())
	defer hub.Close()
	const url = "rtsp://192.0.2.61:554/sub"

	src1 := rtsp.NewSource(url)
	src1.SetVideoTrack(h264Params())
	hub.SetSourceForTest(url, src1)

	sm := NewStreamManager(hub, nil)
	defer sm.Close()
	c1 := sm.getOrCreateConsumer("front", url)
	if src1.ConsumerCount() != 1 {
		t.Fatalf("first source consumer count = %d, want 1", src1.ConsumerCount())
	}

	hub.Remove(url)
	src2 := rtsp.NewSource(url)
	src2.SetVideoTrack(h264Params())
	hub.SetSourceForTest(url, src2)

	c2 := sm.getOrCreateConsumer("front", url)
	if c2 == c1 {
		t.Fatal("new viewer reused the consumer attached to the removed source")
	}
	if src1.ConsumerCount() != 0 {
		t.Errorf("removed source consumer count = %d, want 0", src1.ConsumerCount())
	}
	if src2.ConsumerCount() != 1 {
		t.Errorf("replacement source consumer count = %d, want 1", src2.ConsumerCount())
	}
}

func TestStreamManagerCloseDetachesFromOriginalSource(t *testing.T) {
	hub := rtsp.NewHub(context.Background())
	defer hub.Close()
	const url = "rtsp://192.0.2.63:554/sub"

	src1 := rtsp.NewSource(url)
	src1.SetVideoTrack(h264Params())
	hub.SetSourceForTest(url, src1)
	sm := NewStreamManager(hub, nil)
	sm.getOrCreateConsumer("front", url)

	hub.Remove(url)
	src2 := rtsp.NewSource(url)
	src2.SetVideoTrack(h264Params())
	hub.SetSourceForTest(url, src2)

	sm.Close()
	if src1.ConsumerCount() != 0 {
		t.Errorf("original source consumer count = %d, want 0", src1.ConsumerCount())
	}
	if src2.ConsumerCount() != 0 {
		t.Errorf("replacement source consumer count = %d, want 0", src2.ConsumerCount())
	}
}

func TestStreamManagerStaleCleanupDoesNotRemoveReplacement(t *testing.T) {
	hub := rtsp.NewHub(context.Background())
	defer hub.Close()
	const url = "rtsp://192.0.2.65:554/sub"

	src1 := rtsp.NewSource(url)
	src1.SetVideoTrack(h264Params())
	hub.SetSourceForTest(url, src1)
	sm := NewStreamManager(hub, nil)
	defer sm.Close()
	stale := sm.getOrCreateConsumer("front", url)

	hub.Remove(url)
	src2 := rtsp.NewSource(url)
	src2.SetVideoTrack(h264Params())
	hub.SetSourceForTest(url, src2)
	replacement := sm.getOrCreateConsumer("front", url)

	sm.removeConsumerIfEmpty(url, stale)
	if got := sm.getOrCreateConsumer("front", url); got != replacement {
		t.Fatal("stale cleanup removed the replacement consumer")
	}
	if src2.ConsumerCount() != 1 {
		t.Errorf("replacement source consumer count = %d, want 1", src2.ConsumerCount())
	}
}

// TestRewriteAnswerProfileLevelIDReplacesLevel verifies the typical case where
// Pion echoes Chrome's offered level (1f = 3.1) in the answer while the camera
// emits a higher level (29 = 4.1). The rewrite must substitute the camera's
// PLI so the browser configures its decoder for the bitstream's actual level.
func TestRewriteAnswerProfileLevelIDReplacesLevel(t *testing.T) {
	in := "a=fmtp:96 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64001f\r\n"
	want := "a=fmtp:96 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640029\r\n"
	if got := rewriteAnswerProfileLevelID(in, "640029"); got != want {
		t.Fatalf("rewrite mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// TestRewriteAnswerProfileLevelIDPreservesOtherFmtpParams confirms that the
// surrounding fmtp parameters (level-asymmetry-allowed, packetization-mode)
// and any trailing parameters are untouched.
func TestRewriteAnswerProfileLevelIDPreservesOtherFmtpParams(t *testing.T) {
	in := "a=fmtp:96 packetization-mode=1;profile-level-id=42e01f;sprop-parameter-sets=Z0LAH9oCgPaA,aMuMsg==\r\n"
	got := rewriteAnswerProfileLevelID(in, "42e029")
	want := "a=fmtp:96 packetization-mode=1;profile-level-id=42e029;sprop-parameter-sets=Z0LAH9oCgPaA,aMuMsg==\r\n"
	if got != want {
		t.Fatalf("rewrite mangled surrounding params:\n got: %q\nwant: %q", got, want)
	}
}

// TestRewriteAnswerProfileLevelIDReplacesEveryOccurrence catches the case
// where the SDP contains multiple fmtp lines (e.g. when Pion has emitted both
// the negotiated codec and a redundant retransmission stanza). All should be
// rewritten to keep the bitstream/SDP advertised level consistent.
func TestRewriteAnswerProfileLevelIDReplacesEveryOccurrence(t *testing.T) {
	in := "a=fmtp:96 profile-level-id=64001f\r\n" +
		"a=fmtp:97 profile-level-id=42e01f\r\n"
	got := rewriteAnswerProfileLevelID(in, "640029")
	want := "a=fmtp:96 profile-level-id=640029\r\n" +
		"a=fmtp:97 profile-level-id=640029\r\n"
	if got != want {
		t.Fatalf("rewrite did not replace every occurrence:\n got: %q\nwant: %q", got, want)
	}
}

// TestRewriteAnswerProfileLevelIDNoOpWhenAbsent verifies that an SDP without
// any H.264 fmtp line is returned untouched (e.g. audio-only answers).
func TestRewriteAnswerProfileLevelIDNoOpWhenAbsent(t *testing.T) {
	in := "v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 0\r\na=rtpmap:0 PCMU/8000\r\n"
	if got := rewriteAnswerProfileLevelID(in, "640029"); got != in {
		t.Fatalf("rewrite altered SDP with no profile-level-id:\n got: %q\nwant: %q", got, in)
	}
}

// TestRewriteAnswerProfileLevelIDRejectsMalformedPLI exercises the safety
// guards: malformed input must leave the answer unchanged rather than
// corrupting the SDP with invalid characters.
func TestRewriteAnswerProfileLevelIDRejectsMalformedPLI(t *testing.T) {
	in := "a=fmtp:96 profile-level-id=64001f\r\n"
	for _, bad := range []string{"", "64", "640029ab", "gghhii"} {
		if got := rewriteAnswerProfileLevelID(in, bad); got != in {
			t.Errorf("malformed PLI %q caused rewrite: got %q want %q", bad, got, in)
		}
	}
}

// TestRewriteAnswerProfileLevelIDLeavesCandidatesIntact ensures ICE candidate
// lines and other SDP attributes survive the rewrite unchanged. Pion appends
// candidates after gathering completes, so the rewrite happens with them
// present in the SDP.
func TestRewriteAnswerProfileLevelIDLeavesCandidatesIntact(t *testing.T) {
	in := "v=0\r\n" +
		"m=video 9 UDP/TLS/RTP/SAVPF 96\r\n" +
		"a=rtpmap:96 H264/90000\r\n" +
		"a=fmtp:96 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64001f\r\n" +
		"a=candidate:842163049 1 udp 1677729535 192.0.2.1 50000 typ srflx\r\n"
	want := "v=0\r\n" +
		"m=video 9 UDP/TLS/RTP/SAVPF 96\r\n" +
		"a=rtpmap:96 H264/90000\r\n" +
		"a=fmtp:96 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640029\r\n" +
		"a=candidate:842163049 1 udp 1677729535 192.0.2.1 50000 typ srflx\r\n"
	if got := rewriteAnswerProfileLevelID(in, "640029"); got != want {
		t.Fatalf("rewrite damaged surrounding SDP lines:\n got: %q\nwant: %q", got, want)
	}
}

// TestHandleOfferAudioMLineNotRejected guards against a regression where the
// SDP answer rejected the audio m-line (port=0) whenever the camera's audio
// codec wasn't browser-compatible (e.g. AAC). Pion attaches every ICE
// candidate to the first m-line of the answer; when that m-line was rejected
// it dropped out of the BUNDLE group, stranding the active video m-line with
// no candidates. Chrome's iceConnectionState then stayed at "new" forever and
// the stream stalled in the loading state.
//
// The fix is to register a placeholder audio codec and an inactive audio
// transceiver so the m=audio negotiates with a real port and stays inside
// BUNDLE.
func TestHandleOfferAudioMLineNotRejected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hub := rtsp.NewHub(ctx)
	defer hub.Close()
	sm := NewStreamManager(hub, nil)
	defer sm.Close()

	// Build a client offer that mirrors Chrome's: both audio and video
	// transceivers, recvonly direction.
	client, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("create client pc: %v", err)
	}
	defer func() { _ = client.Close() }()

	if _, err := client.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		t.Fatalf("add audio transceiver: %v", err)
	}
	if _, err := client.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		t.Fatalf("add video transceiver: %v", err)
	}

	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	if err := client.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local description: %v", err)
	}

	// HandleOffer with an unreachable RTSP URL — the Source has no audio
	// track info, which previously triggered the rejected m=audio bug.
	answer, err := sm.HandleOffer("test-cam", "rtsp://invalid:554/stream", offer)
	if err != nil {
		t.Fatalf("HandleOffer: %v", err)
	}

	// Parse the m-lines and confirm audio is NOT rejected.
	mAudioRe := regexp.MustCompile(`(?m)^m=audio (\d+) `)
	m := mAudioRe.FindStringSubmatch(answer.SDP)
	if len(m) != 2 {
		t.Fatalf("no m=audio line in answer SDP:\n%s", answer.SDP)
	}
	if m[1] == "0" {
		t.Fatalf("m=audio port is 0 (rejected); BUNDLE will exclude audio and ICE will fail.\nSDP:\n%s", answer.SDP)
	}

	// Confirm BUNDLE includes both mids.
	bundleRe := regexp.MustCompile(`(?m)^a=group:BUNDLE ([^\r\n]+)`)
	b := bundleRe.FindStringSubmatch(answer.SDP)
	if len(b) != 2 {
		t.Fatalf("no a=group:BUNDLE line in answer SDP:\n%s", answer.SDP)
	}
	mids := strings.Fields(b[1])
	if len(mids) < 2 {
		t.Errorf("BUNDLE group only contains %d mid(s) (%q); expected audio+video both bundled.\nSDP:\n%s", len(mids), b[1], answer.SDP)
	}
}

func TestClampLevelIDC(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"high level 5.1 clamps to 3.1", "640033", "64001f"},
		{"high level 4.1 clamps to 3.1", "640029", "64001f"},
		{"high level 3.1 passes through", "64001f", "64001f"},
		{"high level 2.2 passes through unchanged", "640016", "640016"},
		{"baseline level 3.1 passes through", "42001f", "42001f"},
		{"baseline level 5.0 clamps", "420032", "42001f"},
		{"malformed length passes through", "640", "640"},
		{"malformed hex passes through", "6400zz", "6400zz"},
		{"empty passes through", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clampLevelIDC(tt.in); got != tt.want {
				t.Errorf("clampLevelIDC(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// countStatsLoggerGoroutines returns how many goroutines are currently running
// the peer stats logger, identified by its function name in the stack dump.
func countStatsLoggerGoroutines() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	// The active stack frame of a running stats logger is its closure; this
	// appears exactly once per live goroutine (the "created by" line would
	// double-count).
	return strings.Count(string(buf[:n]), "startStatsLogger.func1(")
}

// TestHandleOffer_ErrorPathCancelsStatsLogger guards against leaking the
// per-peer stats logger goroutine when HandleOffer fails after the logger has
// been started (e.g. SetRemoteDescription rejects a malformed offer).
func TestHandleOffer_ErrorPathCancelsStatsLogger(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hub := rtsp.NewHub(ctx)
	defer hub.Close()

	sm := NewStreamManager(hub, nil)
	defer sm.Close()

	// Pre-seed video params so HandleOffer does not block on WaitForVideoParams.
	const rtspURL = "rtsp://invalid:554/stream"
	src := hub.GetOrCreate(rtspURL)
	src.SetVideoTrack(&rtsp.TrackInfo{
		Codec:   "H264",
		IsVideo: true,
		SPS:     []byte{0x67, 0x64, 0x00, 0x29, 0xac, 0x2c, 0xa5, 0x01, 0x40},
		PPS:     []byte{0x68, 0xee, 0x3c, 0x80},
	})

	// A malformed offer fails at SetRemoteDescription, which runs after the
	// stats logger goroutine has already been started.
	badOffer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: "not-a-valid-sdp"}
	if _, err := sm.HandleOffer("test-cam", rtspURL, badOffer); err == nil {
		t.Fatal("expected HandleOffer to fail on a malformed offer")
	}

	// The error path must cancel the stats logger; allow it a brief moment to
	// observe cancellation and return.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if countStatsLoggerGoroutines() == 0 {
			if src.ConsumerCount() != 0 {
				t.Fatalf("failed offer left %d source consumer(s), want 0", src.ConsumerCount())
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("stats logger goroutine leaked after HandleOffer error: %d still running", countStatsLoggerGoroutines())
}

// blockingWriter blocks inside WriteRTP until release is closed, signalling on
// entered when the call begins. It lets a test hold one peer's video write
// open while probing whether the consumer still serves addPeer/removePeer.
type blockingWriter struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingWriter) WriteRTP(*rtp.Packet) error {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	return nil
}

// OnVideoRTP fans a packet out to every peer. A slow or stuck peer write must
// not hold the consumer lock, or it would block addPeer/removePeer (new
// viewers cannot connect and disconnected viewers cannot be reaped) for as
// long as the write stalls. This asserts the broadcast snapshots the peer set
// and writes outside the lock.
func TestWebrtcConsumer_PeerWriteDoesNotBlockMembershipChanges(t *testing.T) {
	bw := &blockingWriter{entered: make(chan struct{}, 1), release: make(chan struct{})}
	slow := &peerState{video: &trackState{track: bw}, keyframeSeen: true}
	wc := &webrtcConsumer{peers: []*peerState{slow}}

	// Single small NAL (type 1, no fragmentation) → exactly one peer write.
	pkt := &rtp.Packet{Payload: []byte{0x01, 0xAA}}

	go wc.OnVideoRTP(pkt)
	select {
	case <-bw.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("peer write never started")
	}

	done := make(chan struct{})
	go func() {
		wc.addPeer(&peerState{})
		close(done)
	}()
	select {
	case <-done:
		// addPeer completed while the peer write is still blocked: good.
	case <-time.After(2 * time.Second):
		close(bw.release)
		t.Fatal("addPeer blocked while OnVideoRTP held the lock during a slow peer write")
	}
	close(bw.release)
}

func TestStreamManagerClientCounts(t *testing.T) {
	sm := NewStreamManager(nil, nil)
	front := &webrtcConsumer{cameraName: "front"}
	front.peers = []*peerState{{}, {}}
	back := &webrtcConsumer{cameraName: "back"}
	back.peers = []*peerState{{}}
	sm.consumers = map[string]*webrtcConsumer{
		"rtsp://front/main": front,
		"rtsp://back/main":  back,
	}
	got := sm.ClientCounts()
	if got["front"] != 2 || got["back"] != 1 {
		t.Fatalf("ClientCounts() = %v, want front=2 back=1", got)
	}
}
