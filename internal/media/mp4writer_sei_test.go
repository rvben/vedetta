package media

import (
	"bytes"
	"os"
	"testing"
	"time"
)

// The TP-Link SEI payload that started this: a C220 stamps every access unit
// with it, and iOS VideoToolbox rejects the frame as bad data. Live HLS strips
// it, so an iPhone watching the camera is fine while an iPhone opening the
// recorded clip of the same moment hits the identical decoder. The recorder
// must strip it too.
const seiMarker = "TPLINKMARKERBOX"

const idrMarker = "IDR-PAYLOAD"

func seiTestAccessUnit() [][]byte {
	sps := []byte{0x67, 0x42, 0x00, 0x0a, 0xf8, 0x41, 0xa2}
	pps := []byte{0x68, 0xce, 0x38, 0x80}
	sei := append([]byte{0x06, 0x05}, []byte(seiMarker)...)
	idr := append([]byte{0x65, 0x88, 0x84}, []byte(idrMarker)...)
	return [][]byte{sps, pps, sei, idr}
}

func TestSegmentWriter_StripsSEIFromRecordedSamples(t *testing.T) {
	sw := newTestVideoWriter(t, "sei.mp4")

	sw.mu.Lock()
	err := sw.writeVideoAccessUnit(seiTestAccessUnit(), 3000, time.Now())
	sw.mu.Unlock()
	if err != nil {
		t.Fatalf("writeVideoAccessUnit: %v", err)
	}
	if _, err := sw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(sw.path)
	if err != nil {
		t.Fatalf("read segment: %v", err)
	}
	// Two bounds: the picture data must survive, and only the SEI must not.
	// Asserting the absence alone would also pass if the frame were dropped
	// wholesale, which is a different and worse bug.
	if !bytes.Contains(data, []byte(idrMarker)) {
		t.Fatal("recorded segment lost the coded picture")
	}
	if bytes.Contains(data, []byte(seiMarker)) {
		t.Error("recorded segment still carries the camera's SEI payload")
	}
}

// An access unit that holds nothing but SEI has no picture left once stripped.
// Muxing that empty unit would write a zero-length sample; the writer must
// treat it as "no frame here" and carry on.
func TestSegmentWriter_SEIOnlyAccessUnitWritesNothing(t *testing.T) {
	sw := newTestVideoWriter(t, "seionly.mp4")
	defer sw.Close()

	sei := append([]byte{0x06, 0x05}, []byte(seiMarker)...)

	// Open the segment with a real keyframe so the writer is past its
	// wait-for-init guard and would genuinely mux whatever comes next.
	sw.mu.Lock()
	if err := sw.writeVideoAccessUnit(seiTestAccessUnit(), 3000, time.Now()); err != nil {
		sw.mu.Unlock()
		t.Fatalf("keyframe: %v", err)
	}
	before := len(sw.pendingVideoSamples)
	err := sw.writeVideoAccessUnit([][]byte{sei}, 3000, time.Now())
	after := len(sw.pendingVideoSamples)
	sw.mu.Unlock()

	if err != nil {
		t.Fatalf("SEI-only access unit returned an error: %v", err)
	}
	if after != before {
		t.Errorf("SEI-only access unit queued %d sample(s), want 0", after-before)
	}
}
