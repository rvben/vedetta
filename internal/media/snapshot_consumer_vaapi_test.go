//go:build linux && cgo && hwaccel

package media

import (
	"bytes"
	"image"
	"testing"
	"time"
)

// The snapshot consumer hands a VA-API decoder single keyframes with nothing
// between them. Each read has to show the keyframe it took, whether the
// decoder returns the picture from the call that submits the unit or only
// once it is drained.
func TestSnapshotConsumerShowsTheKeyframeItDecodedOnVAAPI(t *testing.T) {
	if !probeVAAPI() {
		t.Skip("no usable VA-API render node")
	}
	units := fixtureKeyframes(t)
	if len(units) < 4 {
		t.Fatalf("fixture has %d keyframes, want at least 4", len(units))
	}
	newDecoder := func() FrameDecoder {
		dec, err := newVAAPIDecoder()
		if err != nil {
			t.Fatalf("VA-API decoder: %v", err)
		}
		return dec
	}

	// What each unit shows: a decoder given nothing else, then drained.
	alone := make([]*image.RGBA, len(units))
	buffered := 0
	for i, unit := range units {
		dec := newDecoder()
		picture := dec.Decode(unit)
		if picture == nil {
			buffered++
			picture = dec.Flush()
		}
		dec.Close()
		if picture == nil {
			t.Fatalf("keyframe %d does not decode", i)
		}
		alone[i] = ycbcrToRGBA(picture)
	}
	t.Logf("%d of %d keyframes were returned only by the drain", buffered, len(units))
	distinct := false
	for i := 1; i < len(alone); i++ {
		distinct = distinct || !bytes.Equal(alone[0].Pix, alone[i].Pix)
	}
	if !distinct {
		t.Fatal("every fixture keyframe decodes to the same picture, so pictures cannot be told apart")
	}

	sc := &SnapshotConsumer{camera: "test", frameDec: newDecoder(), newDecoder: newDecoder}
	defer sc.Close()
	deliver := func(i int) {
		sc.mu.Lock()
		defer sc.mu.Unlock()
		sc.older, sc.unit = sc.unit, keyUnit{data: units[i], at: time.Now()}
	}
	last := len(units) - 1
	for _, i := range []int{0, last, 1, last - 1, last - 1, 2} {
		deliver(i)
		frame := sc.LastFrame()
		if frame == nil {
			t.Fatalf("no frame for keyframe %d", i)
		}
		if !bytes.Equal(frame.Pix, alone[i].Pix) {
			t.Fatalf("the frame read after keyframe %d shows another keyframe", i)
		}
	}

	// A damaged newest unit falls back to the unread one before it, and each
	// later keyframe still shows as itself.
	deliver(3)
	deliver(0)
	sc.mu.Lock()
	sc.unit.data = sc.unit.data[:len(sc.unit.data)/3]
	sc.mu.Unlock()
	if frame := sc.LastFrame(); frame == nil || !bytes.Equal(frame.Pix, alone[3].Pix) {
		t.Fatal("after a damaged keyframe the frame is not the unread keyframe before it")
	}
	deliver(1)
	if frame := sc.LastFrame(); frame == nil || !bytes.Equal(frame.Pix, alone[1].Pix) {
		t.Fatal("the keyframe after a damaged one shows another keyframe")
	}
}
