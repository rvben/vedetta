package media

import (
	"bytes"
	"image"
	"testing"
	"time"
)

// paintingDecoder is a reusing decoder whose pictures are a flat grey of a
// brightness that rises by ten with every picture, so a frame can be told
// apart by any one of its pixels. It records the picture it was asked to
// overwrite.
type paintingDecoder struct {
	w, h    int
	decodes int
	fail    bool
	dsts    []*image.YCbCr
	// before runs ahead of each decode, with the picture about to be overwritten.
	before func(dst *image.YCbCr)
}

func (d *paintingDecoder) Decode(nalData []byte) *image.YCbCr { return d.DecodeInto(nalData, nil) }

func (d *paintingDecoder) DecodeInto(_ []byte, dst *image.YCbCr) *image.YCbCr {
	if d.before != nil {
		d.before(dst)
	}
	d.dsts = append(d.dsts, dst)
	if d.fail {
		return nil
	}
	d.decodes++
	if dst == nil {
		dst = image.NewYCbCr(image.Rect(0, 0, d.w, d.h), image.YCbCrSubsampleRatio420)
	}
	for i := range dst.Y {
		dst.Y[i] = byte(10 * d.decodes)
	}
	for i := range dst.Cb {
		dst.Cb[i], dst.Cr[i] = 128, 128
	}
	return dst
}

func (*paintingDecoder) Flush() *image.YCbCr { return nil }
func (*paintingDecoder) Close()              {}

func samePixels(a, b []byte) bool {
	return len(a) > 0 && len(b) > 0 && &a[0] == &b[0]
}

func TestCopyDecodedFrameOverwritesTheCallersPicture(t *testing.T) {
	const w, h, yStride, cStride = 4, 4, 4, 2
	planes := offHeapPlanes(t, yStride*h, cStride*(h/2))
	for p := range planes {
		for i := range planes[p] {
			planes[p][i] = byte(10*(p+1) + i)
		}
	}
	output := rawOutputWith(planes, 1, w, h, yStride, cStride)

	first := frameFromRawDecoded(&output, nil)
	if first == nil {
		t.Fatal("no picture for a valid frame")
	}
	yBefore, cbBefore, crBefore := first.Y, first.Cb, first.Cr

	for p := range planes {
		for i := range planes[p] {
			planes[p][i] = byte(100 + 10*(p+1) + i)
		}
	}
	second := frameFromRawDecoded(&output, first)
	if second != first {
		t.Fatal("decoding into a picture returned a different picture")
	}
	if !samePixels(second.Y, yBefore) || !samePixels(second.Cb, cbBefore) || !samePixels(second.Cr, crBefore) {
		t.Fatal("decoding into a picture of the same size replaced its planes")
	}
	for p, plane := range [][]byte{second.Y, second.Cb, second.Cr} {
		if !bytes.Equal(plane, planes[p]) {
			t.Fatalf("plane %d = %v, want the new frame %v", p, plane, planes[p])
		}
	}

	allocs := testing.AllocsPerRun(20, func() {
		if frameFromRawDecoded(&output, first) == nil {
			t.Fatal("no picture for a valid frame")
		}
	})
	if allocs != 0 {
		t.Fatalf("decoding into a picture of the same size allocated %v times, want 0", allocs)
	}
}

func TestCopyDecodedFrameFollowsAChangeOfGeometry(t *testing.T) {
	big := offHeapPlanes(t, 8*8, 4*4)
	small := offHeapPlanes(t, 4*4, 2*2)
	for i := range big[0] {
		big[0][i] = 200
	}
	for i := range small[0] {
		small[0][i] = 50
	}
	bigOut := rawOutputWith(big, 1, 8, 8, 8, 4)
	smallOut := rawOutputWith(small, 1, 4, 4, 4, 2)

	img := frameFromRawDecoded(&smallOut, nil)
	img = frameFromRawDecoded(&bigOut, img)
	if img == nil || img.Rect != image.Rect(0, 0, 8, 8) || img.YStride != 8 || img.CStride != 4 {
		t.Fatalf("after growing: %+v", img)
	}
	if len(img.Y) != 64 || len(img.Cb) != 16 || len(img.Cr) != 16 || img.Y[63] != 200 {
		t.Fatalf("grown planes are %d/%d/%d bytes, last luma %d", len(img.Y), len(img.Cb), len(img.Cr), img.Y[len(img.Y)-1])
	}

	img = frameFromRawDecoded(&smallOut, img)
	if img == nil || img.Rect != image.Rect(0, 0, 4, 4) || img.YStride != 4 || img.CStride != 2 {
		t.Fatalf("after shrinking: %+v", img)
	}
	if len(img.Y) != 16 || len(img.Cb) != 4 || len(img.Cr) != 4 || img.Y[15] != 50 {
		t.Fatalf("shrunk planes are %d/%d/%d bytes, last luma %d", len(img.Y), len(img.Cb), len(img.Cr), img.Y[len(img.Y)-1])
	}
}

// A frame the decoder did not produce, or one whose geometry does not fit its
// planes, must leave the caller's last good picture as it was: the caller
// still treats it as the current frame.
func TestCopyDecodedFrameLeavesThePictureAloneWhenRejected(t *testing.T) {
	const w, h, yStride, cStride = 4, 4, 4, 2
	planes := offHeapPlanes(t, yStride*h, cStride*(h/2))
	for i := range planes[0] {
		planes[0][i] = 7
	}
	good := rawOutputWith(planes, 1, w, h, yStride, cStride)
	img := frameFromRawDecoded(&good, nil)
	want := *img
	wantY := append([]byte(nil), img.Y...)

	for i := range planes[0] {
		planes[0][i] = 99
	}
	missing := rawOutputWith(planes, 0, w, h, yStride, cStride)
	if got := frameFromRawDecoded(&missing, img); got != nil {
		t.Fatal("a missing frame produced a picture")
	}
	oversized := rawOutputWith(planes, 1, 8, 8, 8, 4)
	if got := copyDecodedFrame(img, oversized.planes, [3]int{yStride * h, cStride * h / 2, cStride * h / 2}, 8, 8, 8, 4); got != nil {
		t.Fatal("a frame larger than its planes produced a picture")
	}
	if img.Rect != want.Rect || img.YStride != want.YStride || !bytes.Equal(img.Y, wantY) {
		t.Fatalf("rejected frames changed the picture: rect=%v stride=%d Y=%v", img.Rect, img.YStride, img.Y)
	}
}

func TestScaleToRawFrameIntoAUsedBuffer(t *testing.T) {
	img := noiseYCbCr(8, 8, 1)
	size := rawFrameSize(4, 4)
	want := scaleToRawFrame(nil, img, 4, 4).Data
	if len(want) != size {
		t.Fatalf("a 4x4 frame is %d bytes, want %d", len(want), size)
	}

	dirty := bytes.Repeat([]byte{0xAB}, size)
	got := scaleToRawFrame(dirty, img, 4, 4).Data
	if !samePixels(got, dirty) {
		t.Fatal("a buffer of the right size was not reused")
	}
	if !bytes.Equal(got, want) {
		t.Fatal("pixels from the buffer's earlier frame survived the scaling")
	}

	roomy := bytes.Repeat([]byte{0xAB}, size+30)
	got = scaleToRawFrame(roomy, img, 4, 4).Data
	if len(got) != size || !bytes.Equal(got, want) {
		t.Fatalf("a larger buffer gave %d bytes, want exactly %d with the frame's pixels", len(got), size)
	}

	short := make([]byte, size-1)
	got = scaleToRawFrame(short, img, 4, 4).Data
	if samePixels(got, short) || !bytes.Equal(got, want) {
		t.Fatal("a buffer too small for the frame must be replaced, not written past")
	}
}

func TestRawFrameRGB24IntoAUsedBuffer(t *testing.T) {
	frame := scaleToRawFrame(nil, noiseYCbCr(4, 4, 2), 4, 4)
	want := frame.RGB24(nil)

	dirty := bytes.Repeat([]byte{0xAB}, 4*4*3)
	got := frame.RGB24(dirty)
	if !samePixels(got, dirty) {
		t.Fatal("a buffer of the right size was not reused")
	}
	if !bytes.Equal(got, want) {
		t.Fatal("pixels from the buffer's earlier frame survived the conversion")
	}

	roomy := bytes.Repeat([]byte{0xAB}, 4*4*3+30)
	got = frame.RGB24(roomy)
	if len(got) != 4*4*3 || !bytes.Equal(got, want) {
		t.Fatalf("a larger buffer gave %d bytes, want exactly %d with the frame's pixels", len(got), 4*4*3)
	}

	short := make([]byte, 4*4*3-1)
	got = frame.RGB24(short)
	if samePixels(got, short) || !bytes.Equal(got, want) {
		t.Fatal("a buffer too small for the frame must be replaced, not written past")
	}
}

func newReuseDetectConsumer(dec FrameDecoder) *DetectConsumer {
	return &DetectConsumer{
		width:        4,
		height:       4,
		camera:       "reuse",
		h264Dec:      dec,
		sps:          []byte{0x67, 0x42},
		frameCh:      make(chan RawFrame, detectFrameQueue),
		free:         make(chan []byte, detectFrameQueue+1),
		lastLog:      time.Now(),
		fpsWindowDur: 5 * time.Second,
	}
}

// Every access unit is decoded but only some are dispatched, so the decoder is
// handed its previous picture to overwrite instead of allocating one per frame.
func TestDetectConsumerDecodesIntoItsPreviousPicture(t *testing.T) {
	dec := &paintingDecoder{w: 8, h: 8}
	dc := newReuseDetectConsumer(dec)
	dc.frameDelay = time.Hour

	start := time.Now()
	for i := range 4 {
		dc.processAccessUnit([][]byte{{0x65, 0x01}}, start.Add(time.Duration(i)*time.Millisecond))
	}

	if dec.dsts[0] != nil {
		t.Fatal("the first decode had no previous picture to overwrite")
	}
	for i := 1; i < len(dec.dsts); i++ {
		if dec.dsts[i] == nil || dec.dsts[i] != dec.dsts[1] {
			t.Fatalf("decode %d was handed %p, want the picture of the decode before (%p)", i, dec.dsts[i], dec.dsts[1])
		}
	}

	dec.fail = true
	dc.processAccessUnit([][]byte{{0x41, 0x02}}, start.Add(time.Second))
	dec.fail = false
	dc.processAccessUnit([][]byte{{0x41, 0x02}}, start.Add(2*time.Second))
	if last := dec.dsts[len(dec.dsts)-1]; last != dec.dsts[1] {
		t.Fatal("a decode that produced no picture lost the picture to overwrite")
	}
}

func TestDetectConsumerScalesIntoRecycledPixels(t *testing.T) {
	dec := &paintingDecoder{w: 8, h: 8}
	dc := newReuseDetectConsumer(dec)

	at := time.Now()
	next := func() RawFrame {
		t.Helper()
		at = at.Add(time.Second)
		dc.processAccessUnit([][]byte{{0x65, 0x01}}, at)
		select {
		case f := <-dc.Frames():
			return f
		default:
			t.Fatal("no frame dispatched")
			return RawFrame{}
		}
	}

	first := next()
	if first.Data[0] != 10 {
		t.Fatalf("first frame's pixel = %d, want 10", first.Data[0])
	}
	held := next()
	if samePixels(held.Data, first.Data) {
		t.Fatal("a frame the receiver still holds was overwritten by the next one")
	}

	dc.Recycle(first.Data)
	third := next()
	if !samePixels(third.Data, first.Data) {
		t.Fatal("recycled pixels were not reused for the next frame")
	}
	if !third.Valid() || third.Width != 4 || third.Height != 4 {
		t.Fatalf("the consumer dispatched a %dx%d frame of %d bytes, want its 4x4 detection size", third.Width, third.Height, len(third.Data))
	}
	if luma := third.Luma(); luma[0] != 30 || luma[len(luma)-1] != 30 {
		t.Fatalf("frame in recycled pixels reads %d..%d, want the third picture (30)", luma[0], luma[len(luma)-1])
	}
	if held.Data[0] != 20 {
		t.Fatalf("the held frame changed to %d while another was recycled", held.Data[0])
	}
}

// A frame dropped because the detector is behind was never handed out, so its
// pixels go straight back into circulation.
func TestDetectConsumerRecyclesDroppedFrames(t *testing.T) {
	dc := newReuseDetectConsumer(&paintingDecoder{w: 8, h: 8})

	for range detectFrameQueue {
		dc.dispatchFrame(RawFrame{Data: make([]byte, rawFrameSize(4, 4)), Width: 4, Height: 4})
	}
	dropped := make([]byte, rawFrameSize(4, 4))
	dc.dispatchFrame(RawFrame{Data: dropped, Width: 4, Height: 4})

	if got := dc.recycled(); !samePixels(got, dropped) {
		t.Fatal("the dropped frame's pixels were not kept for reuse")
	}
	if got := dc.recycled(); got != nil {
		t.Fatal("pixels of a frame still queued for the detector were offered for reuse")
	}
}

// Recycle is called from the detection goroutine and must never block it, also
// when more buffers come back than can be in circulation.
func TestDetectConsumerRecycleNeverBlocks(t *testing.T) {
	dc := newReuseDetectConsumer(&paintingDecoder{w: 8, h: 8})
	done := make(chan struct{})
	go func() {
		defer close(done)
		dc.Recycle(nil)
		for range 4 * (detectFrameQueue + 1) {
			dc.Recycle(make([]byte, rawFrameSize(4, 4)))
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Recycle blocked")
	}
	if got := dc.recycled(); got == nil {
		t.Fatal("no recycled buffer available after Recycle")
	}
}

// freshFrameDecoder returns a picture of its own for every decode, like the
// hardware backends.
type freshFrameDecoder struct{}

func (freshFrameDecoder) Decode([]byte) *image.YCbCr {
	return image.NewYCbCr(image.Rect(0, 0, 4, 4), image.YCbCrSubsampleRatio420)
}
func (freshFrameDecoder) Flush() *image.YCbCr { return nil }
func (freshFrameDecoder) Close()              {}

// A decoder that cannot decode into a picture would never use the previous
// one, so keeping it between frames only holds memory.
func TestDetectConsumerKeepsNoPictureForADecoderThatCannotReuseIt(t *testing.T) {
	dc := newReuseDetectConsumer(freshFrameDecoder{})

	start := time.Now()
	for i := range 3 {
		dc.processAccessUnit([][]byte{{0x65, 0x01}}, start.Add(time.Duration(i)*time.Second))
	}
	if dc.picture != nil {
		t.Fatal("the last picture is retained although the decoder never reuses it")
	}
	select {
	case frame := <-dc.frameCh:
		if !frame.Valid() {
			t.Fatalf("frame holds %d bytes, which does not fit its 4x4 size", len(frame.Data))
		}
	default:
		t.Fatal("no frame from a decoder that returns its own pictures")
	}
}

func TestDecodeReusingFallsBackToAPlainDecoder(t *testing.T) {
	frame := image.NewYCbCr(image.Rect(0, 0, 2, 2), image.YCbCrSubsampleRatio420)
	plain := &countingFrameDecoder{frame: frame}
	mine := image.NewYCbCr(image.Rect(0, 0, 2, 2), image.YCbCrSubsampleRatio420)
	mine.Y[0] = 42

	if got := decodeReusing(plain, []byte{1}, mine); got != frame {
		t.Fatal("a decoder that cannot reuse a picture must return its own")
	}
	if plain.decodes != 1 || mine.Y[0] != 42 {
		t.Fatalf("decodes=%d, caller's picture luma=%d; want one decode and the picture untouched", plain.decodes, mine.Y[0])
	}

	var dec FrameDecoder = &H264Decoder{}
	if _, ok := dec.(reusingFrameDecoder); !ok {
		t.Fatal("the software decoder no longer decodes into a caller's picture")
	}
}
