package media

import (
	"bytes"
	"image"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"

	"github.com/rvben/vedetta/internal/rtsp"
)

// unitDecoder is a paintingDecoder that also records the units it was given
// and how often it was closed.
type unitDecoder struct {
	paintingDecoder
	units  [][]byte
	closes int
	// damaged is a unit that produces no picture.
	damaged []byte
}

func (d *unitDecoder) Decode(nalData []byte) *image.YCbCr { return d.DecodeInto(nalData, nil) }

func (d *unitDecoder) DecodeInto(nalData []byte, dst *image.YCbCr) *image.YCbCr {
	d.units = append(d.units, nalData)
	if d.damaged != nil && bytes.Equal(nalData, d.damaged) {
		return nil
	}
	return d.paintingDecoder.DecodeInto(nalData, dst)
}

func (d *unitDecoder) Close() { d.closes++ }

// snapshotFeed delivers access units to a consumer as RTP. An access unit is
// complete once a packet with a later timestamp arrives, so each one is
// followed by the start of a predicted frame.
type snapshotFeed struct {
	sc  *SnapshotConsumer
	seq uint16
}

func newSnapshotFeed(t *testing.T, dec FrameDecoder) *snapshotFeed {
	t.Helper()
	h264Format := &format.H264{PayloadTyp: 96, PacketizationMode: 1}
	rtpDec, err := h264Format.CreateDecoder()
	if err != nil {
		t.Fatalf("CreateDecoder: %v", err)
	}
	sc := &SnapshotConsumer{
		camera:      "test",
		frameDec:    dec,
		h264Decoder: rtsp.NewH264AccessUnitDecoder(rtpDec),
	}
	// The parameter sets of the track: SPS 0 and PPS 0.
	for _, set := range [][]byte{{0x67, 0x42, 0, 30, 0x80}, {0x68, 0xce}} {
		if id, ok := sc.rememberParameterSet(set); !ok || id != 0 {
			t.Fatalf("parameter set %x: id=%d ok=%v, want id 0", set, id, ok)
		}
	}
	return &snapshotFeed{sc: sc}
}

func (f *snapshotFeed) packet(payload ...byte) {
	f.seq++
	f.sc.OnVideoRTP(&rtp.Packet{
		Header:  rtp.Header{SequenceNumber: f.seq, Timestamp: uint32(f.seq) * 3000},
		Payload: payload,
	})
}

// keyframe delivers a random-access unit whose slice ends in tag.
func (f *snapshotFeed) keyframe(tag byte) {
	f.packet(0x65, 0x88, 0x84, tag)
	f.packet(0x41, 0x9a, 0x00, 0x00)
}

// recoveryPoint delivers a unit that is random access through a recovery
// point SEI with recovery_frame_cnt 0 ahead of a non-IDR I slice ending in tag.
func (f *snapshotFeed) recoveryPoint(tag byte) {
	f.packet(24, 0, 5, 0x06, 0x06, 0x01, 0x84, 0x80, 0, 4, 0x41, 0x88, 0x84, tag)
	f.packet(0x41, 0x9a, 0x00, 0x00)
}

func keyframeUnit(tag byte) []byte {
	return []byte{0, 0, 0, 1, 0x67, 0x42, 0, 30, 0x80, 0, 0, 0, 1, 0x68, 0xce, 0, 0, 0, 1, 0x65, 0x88, 0x84, tag}
}

// A keyframe nobody asks for is never decoded, and a reader gets the latest
// one, stamped with its arrival.
func TestSnapshotConsumerDecodesOnlyWhenAsked(t *testing.T) {
	dec := &unitDecoder{paintingDecoder: paintingDecoder{w: 4, h: 4}}
	feed := newSnapshotFeed(t, dec)
	sc := feed.sc

	if frame, at := sc.LastFrameAt(); frame != nil || !at.IsZero() {
		t.Fatalf("before any keyframe: frame=%v at=%v, want none", frame, at)
	}

	feed.keyframe(1)
	feed.keyframe(2)
	before := time.Now()
	feed.keyframe(3)
	after := time.Now()
	if len(dec.units) != 0 {
		t.Fatalf("%d keyframes were decoded before anything asked for a frame", len(dec.units))
	}

	frame, at := sc.LastFrameAt()
	if frame == nil || frame.Bounds() != image.Rect(0, 0, 4, 4) {
		t.Fatalf("frame = %v, want a 4x4 image", frame)
	}
	if got := frame.Pix[:4]; !bytes.Equal(got, []byte{10, 10, 10, 255}) {
		t.Fatalf("first pixel = %v, want the first decoded picture's grey", got)
	}
	if len(dec.units) != 1 || !bytes.Equal(dec.units[0], keyframeUnit(3)) {
		t.Fatalf("decoded units = %x, want only the latest keyframe %x", dec.units, keyframeUnit(3))
	}
	if at.Before(before) || at.After(after) {
		t.Fatalf("frame time %v is not the arrival of its keyframe (%v..%v)", at, before, after)
	}

	if again, againAt := sc.LastFrameAt(); again != frame || !againAt.Equal(at) || len(dec.units) != 1 {
		t.Fatal("a second request for the same keyframe decoded it again")
	}
}

// A keyframe that carries its own parameter sets is passed on as it is, and
// they replace the ones from the track for later keyframes.
func TestSnapshotConsumerUsesInBandParameterSets(t *testing.T) {
	dec := &unitDecoder{paintingDecoder: paintingDecoder{w: 4, h: 4}}
	feed := newSnapshotFeed(t, dec)

	// STAP-A: SPS 67 4d, PPS 68 ee, IDR slice.
	feed.packet(24, 0, 5, 0x67, 0x4d, 0, 30, 0x80, 0, 2, 0x68, 0xee, 0, 4, 0x65, 0x88, 0x84, 7)
	feed.packet(0x41, 0x9a, 0x00, 0x00)
	feed.sc.LastFrame()
	feed.keyframe(8)
	feed.sc.LastFrame()

	want := [][]byte{
		{0, 0, 0, 1, 0x67, 0x4d, 0, 30, 0x80, 0, 0, 0, 1, 0x68, 0xee, 0, 0, 0, 1, 0x65, 0x88, 0x84, 7},
		{0, 0, 0, 1, 0x67, 0x4d, 0, 30, 0x80, 0, 0, 0, 1, 0x68, 0xee, 0, 0, 0, 1, 0x65, 0x88, 0x84, 8},
	}
	if len(dec.units) != 2 || !bytes.Equal(dec.units[0], want[0]) || !bytes.Equal(dec.units[1], want[1]) {
		t.Fatalf("decoded units = %x, want %x", dec.units, want)
	}
}

// A unit is decoded without the ones before it, which may never have been
// decoded, so a keyframe that repeats only its SPS still carries the PPS.
func TestSnapshotConsumerCarriesAPPSTheKeyframeLeftOut(t *testing.T) {
	dec := &unitDecoder{paintingDecoder: paintingDecoder{w: 4, h: 4}}
	feed := newSnapshotFeed(t, dec)

	// STAP-A: SPS 67 4e, IDR slice. The PPS (68 ce) is the one from the SDP.
	feed.packet(24, 0, 5, 0x67, 0x4e, 0, 30, 0x80, 0, 4, 0x65, 0x88, 0x84, 7)
	feed.packet(0x41, 0x9a, 0x00, 0x00)
	feed.sc.LastFrame()

	want := []byte{0, 0, 0, 1, 0x67, 0x4e, 0, 30, 0x80, 0, 0, 0, 1, 0x68, 0xce, 0, 0, 0, 1, 0x65, 0x88, 0x84, 7}
	if len(dec.units) != 1 || !bytes.Equal(dec.units[0], want) {
		t.Fatalf("decoded units = %x, want %x", dec.units, want)
	}
}

func TestSnapshotConsumerIgnoresPredictedFrames(t *testing.T) {
	dec := &unitDecoder{paintingDecoder: paintingDecoder{w: 4, h: 4}}
	feed := newSnapshotFeed(t, dec)

	for range 4 {
		feed.packet(0x41, 0x9a, 0x00, 0x00)
	}
	if frame := feed.sc.LastFrame(); frame != nil || len(dec.units) != 0 {
		t.Fatalf("predicted frames produced frame=%v after %d decodes, want neither", frame, len(dec.units))
	}
}

// Every decode goes into the same picture, so a frame that was handed out must
// be a conversion of its own: later decodes may not show through it.
func TestSnapshotConsumerHandedOutFramesNeverChange(t *testing.T) {
	dec := &unitDecoder{paintingDecoder: paintingDecoder{w: 4, h: 4}}
	feed := newSnapshotFeed(t, dec)

	var frames []*image.RGBA
	for i := range 5 {
		feed.keyframe(byte(i))
		frames = append(frames, feed.sc.LastFrame())
	}
	for i, frame := range frames {
		want := byte(10 * (i + 1))
		if frame.Pix[0] != want || frame.Pix[len(frame.Pix)-2] != want {
			t.Fatalf("frame %d now reads %d, want its own picture (%d)", i+1, frame.Pix[0], want)
		}
	}
	if dec.dsts[0] != nil {
		t.Fatal("the first decode has no earlier picture to overwrite")
	}
	for i := 1; i < len(dec.dsts); i++ {
		if dec.dsts[i] == nil || dec.dsts[i] != dec.dsts[1] {
			t.Fatalf("decode %d did not reuse the one picture", i+1)
		}
	}
}

func TestSnapshotConsumerKeepsTheLastFrameWhenDecodeFails(t *testing.T) {
	dec := &unitDecoder{paintingDecoder: paintingDecoder{w: 4, h: 4}}
	feed := newSnapshotFeed(t, dec)
	sc := feed.sc
	feed.keyframe(1)
	frame, at := sc.LastFrameAt()
	if frame == nil {
		t.Fatal("no frame for the first keyframe")
	}

	dec.fail = true
	feed.keyframe(2)
	if got, gotAt := sc.LastFrameAt(); got != frame || !gotAt.Equal(at) {
		t.Fatal("a keyframe that did not decode replaced the last frame or its time")
	}
	if len(dec.units) != 2 {
		t.Fatalf("%d decodes, want the failing keyframe tried once", len(dec.units))
	}
	if sc.LastFrame(); len(dec.units) != 2 {
		t.Fatal("a keyframe that did not decode was tried again")
	}

	dec.fail = false
	feed.keyframe(3)
	got, gotAt := sc.LastFrameAt()
	if got == frame || got.Pix[0] != 20 || !gotAt.After(at) {
		t.Fatalf("after recovering: pixel %d at %v, want the second picture (20) and a later time than %v", got.Pix[0], gotAt, at)
	}
	if frame.Pix[0] != 10 {
		t.Fatalf("the earlier frame changed to %d", frame.Pix[0])
	}
}

// Keyframes pile up unread between snapshots. When the newest one is damaged,
// the one before it is the most recent picture there is.
func TestSnapshotConsumerFallsBackToThePreviousUnreadKeyframe(t *testing.T) {
	dec := &unitDecoder{paintingDecoder: paintingDecoder{w: 4, h: 4}, damaged: keyframeUnit(3)}
	feed := newSnapshotFeed(t, dec)
	sc := feed.sc
	feed.keyframe(1)
	before := time.Now()
	feed.keyframe(2)
	after := time.Now()
	feed.keyframe(3)

	frame, at := sc.LastFrameAt()
	if frame == nil {
		t.Fatal("a damaged newest keyframe left no frame although the one before it was intact")
	}
	if at.Before(before) || at.After(after) {
		t.Fatalf("frame time %v is not the arrival of the keyframe that decoded (%v..%v)", at, before, after)
	}
	want := [][]byte{keyframeUnit(3), keyframeUnit(2)}
	if len(dec.units) != len(want) || !bytes.Equal(dec.units[0], want[0]) || !bytes.Equal(dec.units[1], want[1]) {
		t.Fatalf("decoded %d units, want the newest and then the one before it", len(dec.units))
	}
	if sc.LastFrame(); len(dec.units) != 2 {
		t.Fatal("units were decoded again without a new keyframe")
	}

	// A keyframe that was read is not kept as the fallback of the next one.
	dec.damaged = keyframeUnit(5)
	feed.keyframe(4)
	sc.LastFrame()
	feed.keyframe(5)
	if got, gotAt := sc.LastFrameAt(); got == nil || len(dec.units) != 4 {
		t.Fatalf("%d decodes at %v, want one for each of the two later keyframes", len(dec.units), gotAt)
	}
}

// A decoder that rejected a unit is in an unknown state: OpenH264 goes on to
// reject the intact keyframe that follows a damaged one. The fallback and every
// later unit are decoded by a new decoder.
func TestSnapshotConsumerReplacesADecoderThatFailed(t *testing.T) {
	failed := &unitDecoder{paintingDecoder: paintingDecoder{w: 4, h: 4}, damaged: keyframeUnit(2)}
	fresh := &unitDecoder{paintingDecoder: paintingDecoder{w: 4, h: 4}}
	feed := newSnapshotFeed(t, failed)
	sc := feed.sc
	built := 0
	sc.newDecoder = func() FrameDecoder { built++; return fresh }

	feed.keyframe(1)
	feed.keyframe(2)
	if frame := sc.LastFrame(); frame == nil {
		t.Fatal("no frame from the keyframe before the damaged one")
	}
	if built != 1 || failed.closes != 1 {
		t.Fatalf("after a failed decode: %d decoders built, failed one closed %d times, want 1 and 1", built, failed.closes)
	}
	if len(failed.units) != 1 || len(fresh.units) != 1 || !bytes.Equal(fresh.units[0], keyframeUnit(1)) {
		t.Fatalf("failed decoder got %d units and its replacement %d, want the damaged unit and then the fallback", len(failed.units), len(fresh.units))
	}

	feed.keyframe(3)
	sc.LastFrame()
	if built != 1 || len(fresh.units) != 2 || len(failed.units) != 1 {
		t.Fatal("a working decoder was replaced, or the failed one was used again")
	}
	sc.Close()
	if fresh.closes != 1 || failed.closes != 1 {
		t.Fatalf("Close closed the replacement %d times and the failed decoder %d times, want 1 and 1", fresh.closes, failed.closes)
	}
}

// blockingDecoder holds every decode until released.
type blockingDecoder struct {
	started chan struct{}
	release chan struct{}
}

func (d *blockingDecoder) Decode([]byte) *image.YCbCr {
	d.started <- struct{}{}
	<-d.release
	return image.NewYCbCr(image.Rect(0, 0, 4, 4), image.YCbCrSubsampleRatio420)
}
func (*blockingDecoder) Flush() *image.YCbCr { return nil }
func (*blockingDecoder) Close()              {}

// OnVideoRTP runs on the RTP fan-out goroutine that feeds every consumer of the
// stream, so a reader's decode must never hold it up. The keyframe that arrives
// during a decode is the next one served.
func TestSnapshotConsumerDecodeDoesNotBlockTheStream(t *testing.T) {
	dec := &blockingDecoder{started: make(chan struct{}), release: make(chan struct{})}
	feed := newSnapshotFeed(t, dec)
	sc := feed.sc
	feed.keyframe(1)

	read := make(chan time.Time, 1)
	go func() {
		_, at := sc.LastFrameAt()
		read <- at
	}()
	select {
	case <-dec.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first keyframe was not decoded on request")
	}

	delivered := make(chan struct{})
	go func() {
		defer close(delivered)
		feed.keyframe(2)
	}()
	select {
	case <-delivered:
	case <-time.After(5 * time.Second):
		t.Fatal("a keyframe could not be delivered while another was decoding")
	}

	close(dec.release)
	first := <-read

	second := make(chan time.Time, 1)
	go func() {
		_, at := sc.LastFrameAt()
		second <- at
	}()
	select {
	case <-dec.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the keyframe that arrived during a decode was not decoded on the next request")
	}
	if at := <-second; !at.After(first) {
		t.Fatalf("second frame time %v is not after the first %v", at, first)
	}
}

// A decoder that returns pictures of its own would never use a kept one, so
// holding on to it only holds memory.
func TestSnapshotConsumerKeepsNoPictureForADecoderThatCannotReuseIt(t *testing.T) {
	feed := newSnapshotFeed(t, freshFrameDecoder{})
	for i := range 3 {
		feed.keyframe(byte(i))
		if feed.sc.LastFrame() == nil {
			t.Fatal("no frame from a decoder that returns its own pictures")
		}
	}
	if feed.sc.picture != nil {
		t.Fatal("a picture is retained although the decoder never reuses it")
	}
}

// A consumer may be closed both by an explicit teardown and by a supervising
// shutdown path. The decoder is released once, and a keyframe that arrived
// before the close is not decoded with it afterwards.
func TestSnapshotConsumerClose(t *testing.T) {
	dec := &unitDecoder{paintingDecoder: paintingDecoder{w: 4, h: 4}}
	feed := newSnapshotFeed(t, dec)
	sc := feed.sc
	feed.keyframe(1)
	frame := sc.LastFrame()
	feed.keyframe(2)

	sc.Close()
	sc.Close()
	if dec.closes != 1 {
		t.Fatalf("decoder closed %d times, want once", dec.closes)
	}
	if got := sc.LastFrame(); got != frame {
		t.Errorf("LastFrame after close = %v, want the frame decoded before it", got)
	}
	// A keyframe that arrives after the close is not decoded either.
	feed.keyframe(3)
	if got := sc.LastFrame(); got != frame {
		t.Errorf("LastFrame for a keyframe after close = %v, want the frame decoded before it", got)
	}
	if len(dec.units) != 1 {
		t.Fatalf("%d decodes, want none on a closed decoder", len(dec.units))
	}
}

func TestSnapshotConsumerCloseWithoutDecoder(t *testing.T) {
	frame := image.NewRGBA(image.Rect(0, 0, 2, 2))
	sc := NewTestSnapshotConsumer(frame)
	sc.Close()
	sc.Close()
	if got := sc.LastFrame(); got != frame {
		t.Errorf("LastFrame after close = %v, want cached frame", got)
	}
}

// bufferingDecoder stands in for a decoder that returns nothing for the unit
// it is given and releases its picture only when drained, after which it
// accepts no further input.
type bufferingDecoder struct {
	// grey is the brightness of the picture this decoder releases.
	grey          byte
	fed           int
	held          bool
	drained       bool
	fedAfterDrain int
	closes        int
}

func (d *bufferingDecoder) Decode([]byte) *image.YCbCr {
	if d.drained {
		d.fedAfterDrain++
		return nil
	}
	d.fed++
	d.held = true
	return nil
}

func (d *bufferingDecoder) Flush() *image.YCbCr {
	d.drained = true
	if !d.held {
		return nil
	}
	d.held = false
	picture := image.NewYCbCr(image.Rect(0, 0, 4, 4), image.YCbCrSubsampleRatio420)
	for i := range picture.Y {
		picture.Y[i] = d.grey
	}
	for i := range picture.Cb {
		picture.Cb[i], picture.Cr[i] = 128, 128
	}
	return picture
}

func (d *bufferingDecoder) Close() { d.closes++ }

// A decoder that buffers the unit instead of returning its picture is drained
// for it. Draining ends that decoder's stream, so the next keyframe goes to a
// new one.
func TestSnapshotConsumerDrainsADecoderThatBuffersTheKeyframe(t *testing.T) {
	decoders := []*bufferingDecoder{{grey: 10}, {grey: 50}, {grey: 90}}
	feed := newSnapshotFeed(t, decoders[0])
	sc := feed.sc
	built := 0
	sc.newDecoder = func() FrameDecoder { built++; return decoders[built] }

	feed.keyframe(1)
	first := sc.LastFrame()
	if first == nil || first.Pix[0] != 10 {
		t.Fatalf("first frame = %v, want the picture the first decoder held", first)
	}
	if built != 1 || decoders[0].closes != 1 {
		t.Fatalf("after a drain: built=%d closes=%d, want the drained decoder replaced once", built, decoders[0].closes)
	}

	feed.keyframe(2)
	second := sc.LastFrame()
	if second == nil || second.Pix[0] != 50 {
		t.Fatalf("second frame = %v, want the picture of the second decoder", second)
	}
	if decoders[0].fedAfterDrain != 0 {
		t.Fatal("a drained decoder was given another unit")
	}
	if sc.picture != nil {
		t.Fatal("a picture was kept for a decoder that cannot reuse it")
	}
}

// A slice may refer to a parameter set that arrived with a keyframe nobody
// decoded. A unit carries the PPS its slice names and the SPS that PPS names,
// and no other: a decoder that holds one pair uses the last one it is given.
func TestSnapshotConsumerCarriesTheParameterSetsTheKeyframeRefersTo(t *testing.T) {
	dec := &unitDecoder{paintingDecoder: paintingDecoder{w: 4, h: 4}}
	feed := newSnapshotFeed(t, dec)

	// STAP-A: SPS 1 (67 42 00 1e 40), PPS 1 referring to it (68 48), IDR slice
	// of PPS 1 (65 88 40). Nobody asks for this keyframe.
	feed.packet(24, 0, 5, 0x67, 0x42, 0, 30, 0x40, 0, 2, 0x68, 0x48, 0, 4, 0x65, 0x88, 0x40, 1)
	feed.packet(0x41, 0x9a, 0x00, 0x00)
	// STAP-A: a new PPS 0 (68 ee), IDR slice of PPS 0.
	feed.packet(24, 0, 2, 0x68, 0xee, 0, 4, 0x65, 0x88, 0x84, 2)
	feed.packet(0x41, 0x9a, 0x00, 0x00)
	feed.sc.LastFrame()
	// A bare IDR slice of PPS 0, then one of PPS 1.
	feed.keyframe(3)
	feed.sc.LastFrame()
	feed.packet(0x65, 0x88, 0x40, 4)
	feed.packet(0x41, 0x9a, 0x00, 0x00)
	feed.sc.LastFrame()
	// STAP-A: SPS 1 again, IDR slice of PPS 1: the PPS follows the SPS sent.
	feed.packet(24, 0, 5, 0x67, 0x42, 0, 30, 0x40, 0, 4, 0x65, 0x88, 0x40, 5)
	feed.packet(0x41, 0x9a, 0x00, 0x00)
	feed.sc.LastFrame()

	sps0 := []byte{0, 0, 0, 1, 0x67, 0x42, 0, 30, 0x80}
	sps1 := []byte{0, 0, 0, 1, 0x67, 0x42, 0, 30, 0x40}
	pps1 := []byte{0, 0, 0, 1, 0x68, 0x48}
	newPPS0 := []byte{0, 0, 0, 1, 0x68, 0xee}
	want := [][]byte{
		bytes.Join([][]byte{sps0, newPPS0, {0, 0, 0, 1, 0x65, 0x88, 0x84, 2}}, nil),
		bytes.Join([][]byte{sps0, newPPS0, {0, 0, 0, 1, 0x65, 0x88, 0x84, 3}}, nil),
		bytes.Join([][]byte{sps1, pps1, {0, 0, 0, 1, 0x65, 0x88, 0x40, 4}}, nil),
		bytes.Join([][]byte{sps1, pps1, {0, 0, 0, 1, 0x65, 0x88, 0x40, 5}}, nil),
	}
	if len(dec.units) != len(want) {
		t.Fatalf("decoded units = %x, want %x", dec.units, want)
	}
	for i := range want {
		if !bytes.Equal(dec.units[i], want[i]) {
			t.Fatalf("unit %d = %x, want %x", i, dec.units[i], want[i])
		}
	}
}

// The sets a unit needs are those of its first slice.
func TestSnapshotConsumerReadsTheFirstSliceOfTheKeyframe(t *testing.T) {
	dec := &unitDecoder{paintingDecoder: paintingDecoder{w: 4, h: 4}}
	feed := newSnapshotFeed(t, dec)
	sps0 := []byte{0, 0, 0, 1, 0x67, 0x42, 0, 30, 0x80}
	pps0 := []byte{0, 0, 0, 1, 0x68, 0xce}

	// STAP-A: an IDR slice of PPS 0, then one of PPS 1, which nobody sent.
	feed.packet(24, 0, 4, 0x65, 0x88, 0x84, 1, 0, 4, 0x65, 0x88, 0x40, 1)
	feed.packet(0x41, 0x9a, 0x00, 0x00)
	feed.sc.LastFrame()

	wantUnits(t, "the decoder", dec.units,
		bytes.Join([][]byte{sps0, pps0, {0, 0, 0, 1, 0x65, 0x88, 0x84, 1, 0, 0, 0, 1, 0x65, 0x88, 0x40, 1}}, nil))
}

// A decoder needs the SPS ahead of the PPS that refers to it, whatever order
// the camera sent them in and whatever other sets came along.
func TestSnapshotConsumerPutsTheSPSAheadOfItsPPS(t *testing.T) {
	dec := &unitDecoder{paintingDecoder: paintingDecoder{w: 4, h: 4}}
	feed := newSnapshotFeed(t, dec)

	// STAP-A: PPS 0, SPS 1, SPS 0, IDR slice of PPS 0.
	feed.packet(24, 0, 2, 0x68, 0xce, 0, 5, 0x67, 0x42, 0, 30, 0x40, 0, 5, 0x67, 0x42, 0, 30, 0x80, 0, 4, 0x65, 0x88, 0x84, 1)
	feed.packet(0x41, 0x9a, 0x00, 0x00)
	feed.sc.LastFrame()

	if len(dec.units) != 1 || !bytes.Equal(dec.units[0], keyframeUnit(1)) {
		t.Fatalf("decoded units = %x, want %x", dec.units, keyframeUnit(1))
	}
}

// openGOPSPS is SPS 0 of a real stream, with a frame_num of 4 bits. The feeds
// of the recovery-point tests use it: a recovery-point unit is renumbered, and
// that takes an SPS that can be read in full.
var openGOPSPS = []byte{
	0x67, 0x4d, 0x40, 0x0a, 0xec, 0xa1, 0x42, 0x3f, 0x2e, 0x02, 0x20, 0x00, 0x00,
	0x03, 0x00, 0x20, 0x00, 0x00, 0x03, 0x02, 0x81, 0xe2, 0x44, 0xb2, 0xc0,
}

// newOpenGOPFeed is a feed whose track has openGOPSPS as SPS 0, and a consumer
// that takes its new decoders from the returned list, in order.
func newOpenGOPFeed(t *testing.T, decoders ...*unitDecoder) (*snapshotFeed, *int) {
	t.Helper()
	feed := newSnapshotFeed(t, decoders[0])
	if id, ok := feed.sc.rememberParameterSet(openGOPSPS); !ok || id != 0 {
		t.Fatalf("SPS: id=%d ok=%v, want id 0", id, ok)
	}
	built := new(int)
	feed.sc.newDecoder = func() FrameDecoder {
		*built++
		if *built >= len(decoders) {
			return nil
		}
		return decoders[*built]
	}
	return feed, built
}

func newUnitDecoders(n int) []*unitDecoder {
	decoders := make([]*unitDecoder, n)
	for i := range decoders {
		decoders[i] = &unitDecoder{paintingDecoder: paintingDecoder{w: 4, h: 4}}
	}
	return decoders
}

// openGOPUnit is a unit of a newOpenGOPFeed as the consumer stores it.
func openGOPUnit(nalus ...[]byte) []byte {
	var unit []byte
	for _, nalu := range append([][]byte{openGOPSPS, {0x68, 0xce}}, nalus...) {
		unit = append(append(unit, 0, 0, 0, 1), nalu...)
	}
	return unit
}

func openGOPKeyframe(tag byte) []byte { return openGOPUnit([]byte{0x65, 0x88, 0x84, tag}) }

// openGOPRecoveryPoint is the unit of recoveryPoint(tag) with its frame_num,
// the four bits after the PPS ID, set to 1: 0x84 becomes 0x8c.
func openGOPRecoveryPoint(tag byte) []byte {
	return openGOPUnit([]byte{0x06, 0x06, 0x01, 0x84, 0x80}, []byte{0x41, 0x88, 0x8c, tag})
}

func wantUnits(t *testing.T, name string, got [][]byte, want ...[]byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s decoded %d units, want %d: %x", name, len(got), len(want), got)
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("%s unit %d = %x, want %x", name, i, got[i], want[i])
		}
	}
}

// A decoder takes a recovery-point keyframe only as the picture after an IDR
// picture. So it is decoded by a decoder that has seen nothing else: the latest
// IDR unit first, then the keyframe, renumbered to follow it. That decoder is
// not used again.
func TestSnapshotConsumerDecodesARecoveryPointAfterItsIDR(t *testing.T) {
	decoders := newUnitDecoders(6)
	feed, built := newOpenGOPFeed(t, decoders...)
	sc := feed.sc

	feed.keyframe(1)
	_, idrAt := sc.LastFrameAt()
	wantUnits(t, "the first decoder", decoders[0].units, openGOPKeyframe(1))

	// The first decoder has decoded a unit, so the recovery point gets a new one.
	before := time.Now()
	feed.recoveryPoint(2)
	after := time.Now()
	frame, at := sc.LastFrameAt()
	if frame == nil || at.Before(before) || at.After(after) || !at.After(idrAt) {
		t.Fatalf("frame=%v at %v, want the recovery point that arrived in %v..%v", frame, at, before, after)
	}
	wantUnits(t, "the first decoder", decoders[0].units, openGOPKeyframe(1))
	wantUnits(t, "the second decoder", decoders[1].units, openGOPKeyframe(1), openGOPRecoveryPoint(2))
	if decoders[0].closes != 1 || decoders[1].closes != 1 || *built != 2 {
		t.Fatalf("closes %d and %d, %d decoders built, want both decoders retired and a third in place", decoders[0].closes, decoders[1].closes, *built)
	}
	if frame.Pix[0] != 20 {
		t.Fatalf("pixel %d, want the second picture of the decoder (20), not the IDR picture", frame.Pix[0])
	}

	// A recovery point nobody read is not needed for the one after it, and a
	// decoder that has decoded nothing is used as it is.
	feed.recoveryPoint(3)
	feed.recoveryPoint(4)
	sc.LastFrame()
	wantUnits(t, "the third decoder", decoders[2].units, openGOPKeyframe(1), openGOPRecoveryPoint(4))
	if *built != 3 {
		t.Fatalf("%d decoders built, want 3", *built)
	}

	// A new IDR is decoded on its own and becomes what later recovery points
	// follow, read or not.
	feed.keyframe(5)
	sc.LastFrame()
	feed.keyframe(6)
	feed.recoveryPoint(7)
	sc.LastFrame()
	wantUnits(t, "the fourth decoder", decoders[3].units, openGOPKeyframe(5))
	wantUnits(t, "the fifth decoder", decoders[4].units, openGOPKeyframe(6), openGOPRecoveryPoint(7))
	if decoders[3].closes != 1 || *built != 5 {
		t.Fatalf("closes %d, %d decoders built, want the used decoder replaced for the recovery point", decoders[3].closes, *built)
	}
}

// When the newest recovery point is damaged, the unread keyframe before it is
// decoded the same way, by another new decoder.
func TestSnapshotConsumerFallsBackToThePreviousRecoveryPoint(t *testing.T) {
	decoders := newUnitDecoders(4)
	decoders[0].damaged = openGOPRecoveryPoint(3)
	feed, built := newOpenGOPFeed(t, decoders...)

	feed.keyframe(1)
	before := time.Now()
	feed.recoveryPoint(2)
	after := time.Now()
	feed.recoveryPoint(3)
	frame, at := feed.sc.LastFrameAt()
	if frame == nil || at.Before(before) || at.After(after) {
		t.Fatalf("frame=%v at %v, want the recovery point that arrived in %v..%v", frame, at, before, after)
	}
	wantUnits(t, "the first decoder", decoders[0].units, openGOPKeyframe(1), openGOPRecoveryPoint(3))
	wantUnits(t, "the second decoder", decoders[1].units, openGOPKeyframe(1), openGOPRecoveryPoint(2))
	if *built != 2 {
		t.Fatalf("%d decoders built, want one after the failure and one after the recovery point", *built)
	}
}

// When no unread recovery point decodes, the IDR unit they follow is itself a
// keyframe nobody has seen: it becomes the frame, under its own arrival time.
// Once it is the frame it is not decoded a third time.
func TestSnapshotConsumerFallsBackToTheIDRBehindFailedRecoveryPoints(t *testing.T) {
	decoders := newUnitDecoders(6)
	decoders[0].damaged = openGOPRecoveryPoint(3)
	decoders[1].damaged = openGOPRecoveryPoint(2)
	decoders[3].damaged = openGOPRecoveryPoint(4)
	feed, built := newOpenGOPFeed(t, decoders...)

	before := time.Now()
	feed.keyframe(1)
	after := time.Now()
	feed.recoveryPoint(2)
	feed.recoveryPoint(3)
	frame, at := feed.sc.LastFrameAt()
	if frame == nil || at.Before(before) || at.After(after) {
		t.Fatalf("frame=%v at %v, want the IDR picture that arrived in %v..%v", frame, at, before, after)
	}
	wantUnits(t, "the first decoder", decoders[0].units, openGOPKeyframe(1), openGOPRecoveryPoint(3))
	wantUnits(t, "the second decoder", decoders[1].units, openGOPKeyframe(1), openGOPRecoveryPoint(2))
	wantUnits(t, "the third decoder", decoders[2].units, openGOPKeyframe(1))

	feed.recoveryPoint(4)
	if got, gotAt := feed.sc.LastFrameAt(); got != frame || !gotAt.Equal(at) {
		t.Fatal("the frame changed although the recovery point did not decode")
	}
	wantUnits(t, "the fourth decoder", decoders[3].units, openGOPKeyframe(1), openGOPRecoveryPoint(4))
	if *built != 4 || len(decoders[4].units) != 0 {
		t.Fatalf("%d decoders built and %d units on the last, want the IDR unit left alone once it is the frame", *built, len(decoders[4].units))
	}
}

// A recovery point follows the IDR unit, so without a picture from that unit
// it is not decoded.
func TestSnapshotConsumerLeavesARecoveryPointWhoseIDRDoesNotDecode(t *testing.T) {
	decoders := newUnitDecoders(4)
	for _, dec := range decoders {
		dec.damaged = openGOPKeyframe(1)
	}
	feed, _ := newOpenGOPFeed(t, decoders...)

	feed.keyframe(1)
	feed.recoveryPoint(2)
	if frame := feed.sc.LastFrame(); frame != nil {
		t.Fatal("a frame came from a recovery point whose IDR did not decode")
	}
	wantUnits(t, "the first decoder", decoders[0].units, openGOPKeyframe(1))
	wantUnits(t, "the second decoder", decoders[1].units, openGOPKeyframe(1))
	wantUnits(t, "the third decoder", decoders[2].units, openGOPKeyframe(1))
}

// A decoder that releases the IDR picture only when drained takes no more
// input, so it cannot decode the recovery point after it. The IDR, still
// unread, is the frame.
func TestSnapshotConsumerLeavesARecoveryPointAfterADrainedDecoder(t *testing.T) {
	decoders := []*bufferingDecoder{{grey: 10}, {grey: 20}, {grey: 30}}
	feed := newSnapshotFeed(t, decoders[0])
	feed.sc.rememberParameterSet(openGOPSPS)
	built := 0
	feed.sc.newDecoder = func() FrameDecoder { built++; return decoders[built] }

	feed.keyframe(1)
	feed.recoveryPoint(2)
	if frame := feed.sc.LastFrame(); frame == nil || frame.RGBAAt(0, 0).R != 20 {
		t.Fatalf("frame = %v, want the IDR picture from the second decoder", frame)
	}
	for i, dec := range decoders {
		if dec.fedAfterDrain != 0 || dec.fed > 1 {
			t.Fatalf("decoder %d was fed %d units and %d after its drain, want one unit at most", i, dec.fed, dec.fedAfterDrain)
		}
	}
}

// A decoder that has decoded anything cannot take a recovery point, so without
// a way to build another one the unit is left alone.
func TestSnapshotConsumerLeavesARecoveryPointWithoutANewDecoder(t *testing.T) {
	decoders := newUnitDecoders(1)
	feed, _ := newOpenGOPFeed(t, decoders...)
	feed.sc.newDecoder = nil

	feed.keyframe(1)
	frame, at := feed.sc.LastFrameAt()
	if frame == nil {
		t.Fatal("the IDR unit did not decode")
	}
	feed.recoveryPoint(2)
	if got, gotAt := feed.sc.LastFrameAt(); got != frame || !gotAt.Equal(at) {
		t.Fatal("a recovery point was decoded on a decoder that had decoded another unit")
	}
	wantUnits(t, "the only decoder", decoders[0].units, openGOPKeyframe(1))
}

// A recovery point is only kept when something can decode it: after an IDR
// unit, and with nothing but I slices. A recovery point on a picture with
// inter slices promises a correct picture only to a decoder that had the
// pictures before it.
func TestSnapshotConsumerKeepsOnlyIntraRecoveryPointsAfterAnIDR(t *testing.T) {
	decoders := newUnitDecoders(2)
	feed, built := newOpenGOPFeed(t, decoders...)
	sei := []byte{24, 0, 5, 0x06, 0x06, 0x01, 0x84, 0x80}

	feed.recoveryPoint(1)
	if frame := feed.sc.LastFrame(); frame != nil || len(decoders[0].units) != 0 || *built != 0 {
		t.Fatalf("frame=%v decodes=%d rebuilt=%d for a recovery point without an IDR, want nothing done", frame, len(decoders[0].units), *built)
	}

	feed.keyframe(2)
	frame := feed.sc.LastFrame()
	// A P slice (slice_type 5), then an I slice followed by a P slice, then a
	// P slice followed by an I slice.
	feed.packet(append(sei, 0, 4, 0x41, 0x9a, 0x84, 3)...)
	feed.packet(0x41, 0x9a, 0x00, 0x00)
	feed.packet(append(sei, 0, 4, 0x41, 0x88, 0x84, 4, 0, 4, 0x41, 0x9a, 0x84, 4)...)
	feed.packet(0x41, 0x9a, 0x00, 0x00)
	feed.packet(append(sei, 0, 4, 0x41, 0x9a, 0x84, 4, 0, 4, 0x41, 0x88, 0x84, 4)...)
	feed.packet(0x41, 0x9a, 0x00, 0x00)
	if got := feed.sc.LastFrame(); got != frame || *built != 0 {
		t.Fatalf("a recovery point with inter slices was decoded (%d decoders built)", *built)
	}
	wantUnits(t, "the decoder", decoders[0].units, openGOPKeyframe(2))

	// Two I slices: both are renumbered.
	feed.packet(append(sei, 0, 4, 0x41, 0x88, 0x84, 5, 0, 4, 0x41, 0x88, 0x84, 6)...)
	feed.packet(0x41, 0x9a, 0x00, 0x00)
	if got := feed.sc.LastFrame(); got == frame {
		t.Fatal("an intra recovery point after an IDR was not decoded")
	}
	wantUnits(t, "the second decoder", decoders[1].units, openGOPKeyframe(2), openGOPUnit(
		[]byte{0x06, 0x06, 0x01, 0x84, 0x80}, []byte{0x41, 0x88, 0x8c, 5}, []byte{0x41, 0x88, 0x8c, 6}))
}

// A recovery point that cannot be renumbered, or that has no new decoder to go
// to, costs nothing: the decoder in place is kept and the frame stays.
func TestSnapshotConsumerLeavesARecoveryPointItCannotDecode(t *testing.T) {
	// The SPS of this feed is cut off, so the width of frame_num is unknown.
	dec := &unitDecoder{paintingDecoder: paintingDecoder{w: 4, h: 4}}
	feed := newSnapshotFeed(t, dec)
	built := 0
	feed.sc.newDecoder = func() FrameDecoder { built++; return dec }
	feed.keyframe(1)
	frame := feed.sc.LastFrame()
	feed.recoveryPoint(2)
	if got := feed.sc.LastFrame(); got != frame || built != 0 || len(dec.units) != 1 || dec.closes != 0 {
		t.Fatalf("decodes=%d rebuilt=%d closes=%d, want the unit left alone", len(dec.units), built, dec.closes)
	}

	// No new decoder can be built: the used one is not given the unit.
	decoders := newUnitDecoders(1)
	open, _ := newOpenGOPFeed(t, decoders...)
	open.keyframe(1)
	frame = open.sc.LastFrame()
	open.recoveryPoint(2)
	if got := open.sc.LastFrame(); got != frame || len(decoders[0].units) != 1 {
		t.Fatalf("%d units decoded, want the recovery point kept from a decoder that decoded another unit", len(decoders[0].units))
	}
}

// A keyframe whose slice names a PPS nobody sent cannot be decoded and is not
// kept, and neither is one whose slice header cannot be read.
func TestSnapshotConsumerSkipsAKeyframeWithUnknownParameterSets(t *testing.T) {
	dec := &unitDecoder{paintingDecoder: paintingDecoder{w: 4, h: 4}}
	feed := newSnapshotFeed(t, dec)

	feed.packet(0x65, 0x88, 0x40, 1) // PPS 1
	feed.packet(0x41, 0x9a, 0x00, 0x00)
	feed.packet(0x65, 0x00) // header cut off
	feed.packet(0x41, 0x9a, 0x00, 0x01)
	// STAP-A: PPS 1 referring to SPS 1, which nobody sent, and its IDR slice.
	feed.packet(24, 0, 2, 0x68, 0x48, 0, 4, 0x65, 0x88, 0x40, 2)
	feed.packet(0x41, 0x9a, 0x00, 0x02)
	if frame := feed.sc.LastFrame(); frame != nil || len(dec.units) != 0 {
		t.Fatalf("frame=%v decodes=%d for keyframes without usable parameter sets, want none", frame, len(dec.units))
	}
}

func TestReferencedParameterSetIDs(t *testing.T) {
	slices := []struct {
		name string
		nalu []byte
		want uint32
		ok   bool
	}{
		{"PPS 0", []byte{0x65, 0x88, 0x84}, 0, true},
		{"PPS 1", []byte{0x65, 0x88, 0x40}, 1, true},
		// first_mb_in_slice 0, slice_type 7, then 255 as 00000000 1 00000000.
		{"PPS 255", []byte{0x65, 0x88, 0x00, 0x80, 0x00}, 255, true},
		{"PPS 256 is out of range", []byte{0x65, 0x88, 0x00, 0x80, 0x80}, 0, false},
		{"cut off before the PPS ID", []byte{0x65, 0x88}, 0, false},
		// first_mb_in_slice 3 (00100) and slice_type 2 (011) fill the byte.
		{"ends where the PPS ID starts", []byte{0x65, 0x23}, 0, false},
		{"no payload", []byte{0x65}, 0, false},
		{"empty", nil, 0, false},
	}
	for _, tc := range slices {
		if got, ok := sliceParameterSetID(tc.nalu); got != tc.want || ok != tc.ok {
			t.Errorf("slice %s: got %d, %v, want %d, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
	sets := []struct {
		name string
		nalu []byte
		want uint32
		ok   bool
	}{
		{"PPS 0 of SPS 0", []byte{0x68, 0xce}, 0, true},
		{"PPS 1 of SPS 1", []byte{0x68, 0x48}, 1, true},
		// PPS 0, then 31 as 00000 100000.
		{"SPS 31", []byte{0x68, 0x82, 0x00}, 31, true},
		{"SPS 32 is out of range", []byte{0x68, 0x82, 0x10}, 0, false},
		{"cut off after its own ID", []byte{0x68, 0x80}, 0, false},
	}
	for _, tc := range sets {
		if got, ok := ppsSequenceID(tc.nalu); got != tc.want || ok != tc.ok {
			t.Errorf("PPS %s: got %d, %v, want %d, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// A drained decoder accepts no more input. When its replacement cannot be
// built it is retired all the same, and the next keyframe gets a new decoder
// instead of being fed to the drained one.
func TestSnapshotConsumerRetiresASpentDecoderItCannotReplace(t *testing.T) {
	spent := &bufferingDecoder{grey: 10}
	fresh := &bufferingDecoder{grey: 20}
	available := false
	feed := newSnapshotFeed(t, spent)
	feed.sc.newDecoder = func() FrameDecoder {
		if !available {
			return nil
		}
		return fresh
	}

	feed.keyframe(1)
	if frame := feed.sc.LastFrame(); frame == nil || frame.RGBAAt(0, 0).R != 10 {
		t.Fatalf("first keyframe: frame=%v, want the drained picture", frame)
	}
	if spent.closes != 1 {
		t.Fatal("the drained decoder was kept")
	}
	// No decoder can be built: the keyframe is lost, the frame stays.
	feed.keyframe(2)
	if frame := feed.sc.LastFrame(); frame == nil || frame.RGBAAt(0, 0).R != 10 {
		t.Fatal("without a decoder the last frame did not stay in place")
	}
	available = true
	feed.keyframe(3)
	if frame := feed.sc.LastFrame(); frame == nil || frame.RGBAAt(0, 0).R != 20 {
		t.Fatal("the keyframe after a decoder became available was not decoded by it")
	}
	if spent.fedAfterDrain != 0 {
		t.Fatalf("the drained decoder was fed %d more units", spent.fedAfterDrain)
	}
}

// A closed consumer cannot decode, so it does not hold on to keyframes.
func TestSnapshotConsumerCloseDropsUnreadKeyframes(t *testing.T) {
	feed := newSnapshotFeed(t, &unitDecoder{paintingDecoder: paintingDecoder{w: 4, h: 4}})
	feed.keyframe(1)
	feed.keyframe(2)
	feed.recoveryPoint(3)
	feed.keyframe(4)
	feed.keyframe(5)
	feed.sc.mu.Lock()
	unread := feed.sc.unit.data != nil && feed.sc.older.data != nil
	feed.sc.mu.Unlock()
	if !unread {
		t.Fatal("two unread keyframes expected before the close")
	}
	feed.sc.Close()
	feed.sc.mu.Lock()
	defer feed.sc.mu.Unlock()
	if feed.sc.unit.data != nil || feed.sc.older.data != nil {
		t.Fatal("a closed consumer kept unread keyframes")
	}
}

func TestParameterSetID(t *testing.T) {
	sc := &SnapshotConsumer{}
	cases := []struct {
		name   string
		nalu   []byte
		wantID uint32
		wantOK bool
	}{
		{"SPS 0", []byte{0x67, 0x42, 0, 30, 0x80}, 0, true},
		{"SPS 31", []byte{0x67, 0x42, 0, 30, 0x04, 0x00}, 31, true},
		{"SPS 32 is out of range", []byte{0x67, 0x42, 0, 30, 0x04, 0x20}, 0, false},
		{"SPS cut off before its ID", []byte{0x67, 0x42, 0, 30}, 0, false},
		{"SPS cut off inside its ID", []byte{0x67, 0x42, 0, 30, 0x04}, 0, false},
		// 00 00 03 hides an emulation prevention byte: the ID starts after it.
		{"SPS 0 after an emulation prevention byte", []byte{0x67, 0x42, 0, 0, 3, 0x80}, 0, true},
		{"PPS 0", []byte{0x68, 0xce}, 0, true},
		{"PPS 1", []byte{0x68, 0x48}, 1, true},
		{"PPS 255", []byte{0x68, 0x00, 0x80, 0x00}, 255, true},
		{"PPS 256 is out of range", []byte{0x68, 0x00, 0x80, 0x80}, 0, false},
		{"PPS with no payload", []byte{0x68}, 0, false},
		{"a slice is not a parameter set", []byte{0x65, 0x88}, 0, false},
		{"empty", nil, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, ok := sc.rememberParameterSet(tc.nalu)
			if id != tc.wantID || ok != tc.wantOK {
				t.Fatalf("id=%d ok=%v, want id=%d ok=%v", id, ok, tc.wantID, tc.wantOK)
			}
		})
	}
	if !bytes.Equal(sc.sps[31], []byte{0x67, 0x42, 0, 30, 0x04, 0x00}) || !bytes.Equal(sc.pps[255], []byte{0x68, 0x00, 0x80, 0x00}) {
		t.Fatal("a readable parameter set was not stored under its ID")
	}
	stored := 0
	for _, set := range sc.sps {
		if set != nil {
			stored++
		}
	}
	for _, set := range sc.pps {
		if set != nil {
			stored++
		}
	}
	if stored != 5 {
		t.Fatalf("%d parameter sets stored, want the 5 readable ones", stored)
	}
}

// Without any SPS, from the track or the stream, a keyframe cannot be decoded
// and is not kept.
func TestSnapshotConsumerSkipsAKeyframeWithoutAnySPS(t *testing.T) {
	dec := &unitDecoder{paintingDecoder: paintingDecoder{w: 4, h: 4}}
	feed := newSnapshotFeed(t, dec)
	feed.sc.sps = [maxSPSID + 1][]byte{}

	feed.keyframe(1)
	if frame := feed.sc.LastFrame(); frame != nil || len(dec.units) != 0 {
		t.Fatalf("frame=%v decodes=%d for a keyframe without an SPS, want none", frame, len(dec.units))
	}
}
