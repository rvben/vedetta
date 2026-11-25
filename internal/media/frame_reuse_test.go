package media

import (
	"bytes"
	"image"
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"

	"github.com/rvben/vedetta/internal/rtsp"
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

func TestYCbCrToRGB24ScaledIntoAUsedBuffer(t *testing.T) {
	img := createTestYCbCr(8, 8)
	want := ycbcrToRGB24Scaled(nil, img, 4, 4)

	dirty := bytes.Repeat([]byte{0xAB}, 4*4*3)
	got := ycbcrToRGB24Scaled(dirty, img, 4, 4)
	if !samePixels(got, dirty) {
		t.Fatal("a buffer of the right size was not reused")
	}
	if !bytes.Equal(got, want) {
		t.Fatal("pixels from the buffer's earlier frame survived the conversion")
	}

	roomy := bytes.Repeat([]byte{0xAB}, 4*4*3+30)
	got = ycbcrToRGB24Scaled(roomy, img, 4, 4)
	if len(got) != 4*4*3 || !bytes.Equal(got, want) {
		t.Fatalf("a larger buffer gave %d bytes, want exactly %d with the frame's pixels", len(got), 4*4*3)
	}

	short := make([]byte, 4*4*3-1)
	got = ycbcrToRGB24Scaled(short, img, 4, 4)
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

// Every access unit is decoded but only some are converted, so the decoder is
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

func TestDetectConsumerConvertsIntoRecycledPixels(t *testing.T) {
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
	if third.Data[0] != 30 || third.Data[len(third.Data)-1] != 30 {
		t.Fatalf("frame in recycled pixels reads %d..%d, want the third picture (30)", third.Data[0], third.Data[len(third.Data)-1])
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
		dc.dispatchFrame(RawFrame{Data: make([]byte, 48), Width: 4, Height: 4})
	}
	dropped := make([]byte, 48)
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
			dc.Recycle(make([]byte, 48))
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

func newReuseSnapshotConsumer(dec FrameDecoder) *SnapshotConsumer {
	return &SnapshotConsumer{
		camera:   "reuse",
		frameDec: dec,
		decodeCh: make(chan []byte, 1),
		done:     make(chan struct{}),
	}
}

func TestSnapshotConsumerConvertsOnceOnRequest(t *testing.T) {
	dec := &paintingDecoder{w: 4, h: 4}
	sc := newReuseSnapshotConsumer(dec)

	if frame, at := sc.LastFrameAt(); frame != nil || !at.IsZero() {
		t.Fatalf("before any decode: frame=%v at=%v, want none", frame, at)
	}

	before := time.Now()
	sc.decode([]byte{1})
	if sc.lastFrame != nil {
		t.Fatal("the frame was converted before anything asked for it")
	}

	frame, at := sc.LastFrameAt()
	if frame == nil || frame.Bounds() != image.Rect(0, 0, 4, 4) {
		t.Fatalf("frame = %v, want a 4x4 image", frame)
	}
	if got := frame.Pix[:4]; !bytes.Equal(got, []byte{10, 10, 10, 255}) {
		t.Fatalf("first pixel = %v, want the first picture's grey", got)
	}
	if at.Before(before) || at.After(time.Now()) {
		t.Fatalf("decode time %v is outside the decode", at)
	}
	if again, againAt := sc.LastFrameAt(); again != frame || !againAt.Equal(at) {
		t.Fatal("a second request for the same frame converted it again")
	}
}

// Pictures alternate between two buffers, so a frame that was handed out must
// be a conversion of its own: later decodes may not show through it.
func TestSnapshotConsumerHandedOutFramesNeverChange(t *testing.T) {
	dec := &paintingDecoder{w: 4, h: 4}
	sc := newReuseSnapshotConsumer(dec)

	var frames []*image.RGBA
	for range 5 {
		sc.decode([]byte{1})
		frames = append(frames, sc.LastFrame())
	}
	for i, frame := range frames {
		want := byte(10 * (i + 1))
		if frame.Pix[0] != want || frame.Pix[len(frame.Pix)-2] != want {
			t.Fatalf("frame %d now reads %d, want its own picture (%d)", i+1, frame.Pix[0], want)
		}
	}
}

// The decoder may only overwrite the picture before the published one. Handing
// it the published picture would let a reader convert a half-written frame.
func TestSnapshotConsumerNeverDecodesIntoThePublishedPicture(t *testing.T) {
	dec := &paintingDecoder{w: 4, h: 4}
	sc := newReuseSnapshotConsumer(dec)
	dec.before = func(dst *image.YCbCr) {
		sc.mu.Lock()
		defer sc.mu.Unlock()
		if dst != nil && dst == sc.picture {
			t.Error("the decoder was handed the published picture")
		}
	}

	for range 6 {
		sc.decode([]byte{1})
	}
	if dec.dsts[0] != nil || dec.dsts[1] != nil {
		t.Fatal("the first two decodes have no earlier picture to overwrite")
	}
	for i := 2; i < len(dec.dsts); i++ {
		if dec.dsts[i] == nil {
			t.Fatalf("decode %d allocated a picture although one was spare", i+1)
		}
	}
	if dec.dsts[2] == dec.dsts[3] || dec.dsts[2] != dec.dsts[4] || dec.dsts[3] != dec.dsts[5] {
		t.Fatal("the two pictures do not alternate")
	}
}

func TestSnapshotConsumerKeepsTheLastFrameWhenDecodeFails(t *testing.T) {
	dec := &paintingDecoder{w: 4, h: 4}
	sc := newReuseSnapshotConsumer(dec)
	sc.decode([]byte{1})
	sc.decode([]byte{1})
	frame, at := sc.LastFrameAt()

	dec.fail = true
	sc.decode([]byte{1})
	if got, gotAt := sc.LastFrameAt(); got != frame || !gotAt.Equal(at) {
		t.Fatal("a decode that produced no picture replaced the last frame")
	}

	dec.fail = false
	sc.decode([]byte{1})
	if got := sc.LastFrame(); got == frame || got.Pix[0] != 30 {
		t.Fatalf("after recovering, the frame reads %d, want the third picture (30)", got.Pix[0])
	}
	if frame.Pix[0] != 20 {
		t.Fatalf("the earlier frame changed to %d", frame.Pix[0])
	}
}

// At most one random-access unit a second is queued for decode, counted from
// the last decoded frame.
func TestSnapshotConsumerQueuesOneDecodePerSecond(t *testing.T) {
	h264Format := &format.H264{PayloadTyp: 96, PacketizationMode: 1}
	rtpDec, err := h264Format.CreateDecoder()
	if err != nil {
		t.Fatalf("CreateDecoder: %v", err)
	}
	sc := newReuseSnapshotConsumer(&paintingDecoder{w: 4, h: 4})
	sc.h264Decoder = rtsp.NewH264AccessUnitDecoder(rtpDec)
	sc.sps = []byte{0x67, 0x42}

	// An access unit is complete once a packet with a later timestamp arrives,
	// so each random-access unit is followed by the start of a predicted frame.
	var seq uint16
	packet := func(payload ...byte) *rtp.Packet {
		seq++
		return &rtp.Packet{
			Header:  rtp.Header{SequenceNumber: seq, Timestamp: uint32(seq) * 3000},
			Payload: payload,
		}
	}
	deliverIDR := func() {
		sc.OnVideoRTP(packet(0x65, 0x88, 0x84, 0x00))
		sc.OnVideoRTP(packet(0x41, 0x9a, 0x00, 0x00))
	}
	queued := func() bool {
		select {
		case <-sc.decodeCh:
			return true
		default:
			return false
		}
	}

	deliverIDR()
	if !queued() {
		t.Fatal("the first random-access unit was not queued")
	}

	sc.decode([]byte{1})
	deliverIDR()
	if queued() {
		t.Fatal("a random-access unit right after a decoded frame was queued")
	}

	earlier := time.Now().Add(-1500 * time.Millisecond)
	sc.lastDecode.Store(&earlier)
	deliverIDR()
	if !queued() {
		t.Fatal("a random-access unit more than a second after the last frame was not queued")
	}
}

// The interval is measured on the monotonic clock: a wall clock stepped back
// by NTP or by hand would otherwise hold snapshots still until it caught up.
// time.Time prints its monotonic reading as "m=", and loses it when rebuilt
// from a wall-clock value.
func TestSnapshotConsumerTimesDecodesOnTheMonotonicClock(t *testing.T) {
	sc := newReuseSnapshotConsumer(&paintingDecoder{w: 4, h: 4})
	sc.decode([]byte{1})

	stamp := sc.lastDecode.Load()
	if stamp == nil {
		t.Fatal("a decoded frame left no timestamp")
	}
	if !strings.Contains(stamp.String(), " m=") {
		t.Fatalf("decode timestamp %v has no monotonic reading", stamp)
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
		if len(frame.Data) != 4*4*3 {
			t.Fatalf("frame holds %d bytes, want %d", len(frame.Data), 4*4*3)
		}
	default:
		t.Fatal("no frame from a decoder that returns its own pictures")
	}
}

// A decoder that cannot decode into a picture would never use a spare one, so
// keeping the previous frame for it only holds memory.
func TestSnapshotConsumerKeepsNoSpareForADecoderThatCannotReuseIt(t *testing.T) {
	sc := newReuseSnapshotConsumer(freshFrameDecoder{})
	for range 3 {
		sc.decode([]byte{1})
	}
	if sc.spare != nil {
		t.Fatal("the previous picture is retained although the decoder never reuses it")
	}
	if sc.LastFrame() == nil {
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
