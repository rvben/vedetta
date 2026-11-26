package media

import (
	"bytes"
	"image"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"

	"github.com/rvben/vedetta/internal/rtsp"
)

// fixtureKeyframes returns the random-access units of the committed recording,
// each as the Annex B stream the snapshot consumer hands its decoder.
func fixtureKeyframes(t *testing.T) [][]byte {
	t.Helper()
	src, err := openTranscodeSource(filepath.Join("testdata", "sample_segment.mp4"))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer src.file.Close()

	var units [][]byte
	for i, blk := range src.blocks {
		parts, err := readGOPBlock(src.file, blk, i)
		if err != nil {
			t.Fatalf("read block %d: %v", i, err)
		}
		gop := splitGOPTracks(parts, src.videoTrackID, src.audioTrackID)
		if len(gop.videoAVCC) == 0 {
			continue
		}
		annexB, err := avccToAnnexB(gop.videoAVCC)
		if err != nil {
			t.Fatalf("block %d: %v", i, err)
		}
		unit := extractIDRAccessUnit(reorderAnnexBWithSPSPPS(annexB, src.h264.SPS, src.h264.PPS))
		if len(unit) > 0 {
			units = append(units, unit)
		}
	}
	return units
}

// The snapshot consumer decodes keyframes with arbitrary gaps between them
// and, after a damaged one, out of order. Each read has to show the keyframe
// it took: OpenH264 holds a picture back, so without draining it a read would
// return the keyframe of the previous read under the arrival time of this one.
func TestSnapshotConsumerShowsTheKeyframeItDecoded(t *testing.T) {
	if !ensureOpenH264() {
		t.Skip("OpenH264 not available")
	}
	units := fixtureKeyframes(t)
	if len(units) < 4 {
		t.Fatalf("fixture has %d keyframes, want at least 4", len(units))
	}

	// What each unit shows: a decoder given nothing else, then drained.
	alone := make([]*image.RGBA, len(units))
	for i, unit := range units {
		dec := NewH264Decoder()
		picture := dec.Decode(unit)
		if held := dec.Flush(); held != nil {
			picture = held
		}
		dec.Close()
		if picture == nil {
			t.Fatalf("keyframe %d does not decode", i)
		}
		alone[i] = ycbcrToRGBA(picture)
	}
	distinct := false
	for i := 1; i < len(alone); i++ {
		distinct = distinct || !bytes.Equal(alone[0].Pix, alone[i].Pix)
	}
	if !distinct {
		t.Fatal("every fixture keyframe decodes to the same picture, so pictures cannot be told apart")
	}

	sc := &SnapshotConsumer{
		camera:     "test",
		frameDec:   NewH264Decoder(),
		newDecoder: func() FrameDecoder { return NewH264Decoder() },
	}
	defer sc.Close()
	deliver := func(i int) {
		sc.mu.Lock()
		defer sc.mu.Unlock()
		sc.seq++
		sc.older, sc.unit = sc.unit, keyUnit{data: units[i], at: time.Now(), seq: sc.seq}
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

	// A damaged newest unit falls back to the unread one before it, and the
	// decoder then still shows each later keyframe as itself.
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

// A decoder that was fed a unit without being drained still holds that unit's
// picture. A keyframe request returns the picture of the keyframe it submits,
// not the held one.
func TestDecodeKeyframeIntoSkipsAHeldPicture(t *testing.T) {
	if !ensureOpenH264() {
		t.Skip("OpenH264 not available")
	}
	units := fixtureKeyframes(t)
	reference := func(unit []byte) []byte {
		dec := NewH264Decoder()
		defer dec.Close()
		picture := dec.Decode(unit)
		if held := dec.Flush(); held != nil {
			picture = held
		}
		if picture == nil {
			t.Fatal("the fixture keyframe did not decode")
		}
		return ycbcrToRGBA(picture).Pix
	}
	first, last := reference(units[0]), reference(units[len(units)-1])
	if bytes.Equal(first, last) {
		t.Fatal("the fixture keyframes are identical, so the test cannot tell them apart")
	}

	dec := NewH264Decoder()
	defer dec.Close()
	dec.Decode(units[0])
	picture := dec.DecodeKeyframeInto(units[len(units)-1], nil)
	if picture == nil {
		t.Fatal("no picture for the requested keyframe")
	}
	if got := ycbcrToRGBA(picture).Pix; !bytes.Equal(got, last) {
		t.Fatalf("picture of the requested keyframe expected, held picture returned: %v", bytes.Equal(got, first))
	}

	// A unit the decoder rejects has no picture. The one held from the unit
	// before it must not be returned in its place.
	holding := NewH264Decoder()
	defer holding.Close()
	holding.Decode(units[0])
	damaged := units[len(units)-1]
	if picture := holding.DecodeKeyframeInto(damaged[:len(damaged)/3], nil); picture != nil {
		t.Fatalf("a damaged keyframe produced a picture; it is the held one: %v", bytes.Equal(ycbcrToRGBA(picture).Pix, first))
	}
}

// openGOPAccessUnits returns the access units of an open-GOP stream: one IDR
// keyframe, then keyframes that are recovery points ahead of a non-IDR slice.
// Every access unit starts with an access unit delimiter.
func openGOPAccessUnits(t *testing.T) [][][]byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "open_gop.h264"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var units [][][]byte
	for _, nalu := range splitAnnexB(raw) {
		if h264.NALUType(nalu[0]&0x1f) == h264.NALUTypeAccessUnitDelimiter {
			units = append(units, nil)
		}
		units[len(units)-1] = append(units[len(units)-1], nalu)
	}
	return units
}

// A camera with an open GOP sends one IDR and then keyframes that are recovery
// points. OpenH264 decodes such a keyframe only after every keyframe since the
// IDR, so the consumer decodes them in order, whether or not the IDR was read.
func TestSnapshotConsumerShowsARecoveryPointKeyframe(t *testing.T) {
	if !ensureOpenH264() {
		t.Skip("OpenH264 not available")
	}
	units := openGOPAccessUnits(t)

	// The keyframes, each as the camera sent it, and what each shows: a
	// decoder given every keyframe up to it.
	var keyframes [][]byte
	for _, unit := range units {
		var annexB []byte
		key := false
		for _, nalu := range unit {
			key = key || h264.NALUType(nalu[0]&0x1f) == h264.NALUTypeSPS
			annexB = append(append(annexB, 0, 0, 0, 1), nalu...)
		}
		if key {
			keyframes = append(keyframes, annexB)
		}
	}
	if len(keyframes) != 3 {
		t.Fatalf("fixture has %d keyframes, want 3", len(keyframes))
	}
	reference := NewH264Decoder()
	defer reference.Close()
	shows := make([][]byte, len(keyframes))
	for i, keyframe := range keyframes {
		picture := reference.DecodeKeyframeInto(keyframe, nil)
		if picture == nil {
			t.Fatalf("keyframe %d does not decode after the ones before it", i)
		}
		shows[i] = ycbcrToRGBA(picture).Pix
	}
	if bytes.Equal(shows[0], shows[1]) || bytes.Equal(shows[1], shows[2]) || bytes.Equal(shows[0], shows[2]) {
		t.Fatal("fixture keyframes show the same picture, so they cannot be told apart")
	}

	// The fixture has what the test is about: its last keyframe decodes
	// neither on its own nor straight after the IDR.
	for _, before := range [][]byte{nil, keyframes[0]} {
		dec := NewH264Decoder()
		if before != nil && dec.DecodeKeyframeInto(before, nil) == nil {
			t.Fatal("the IDR keyframe does not decode")
		}
		picture := dec.DecodeKeyframeInto(keyframes[2], nil)
		dec.Close()
		if picture != nil {
			t.Fatal("the last keyframe decodes without the one before it, so the fixture proves nothing")
		}
	}

	// readAfter lists the keyframes after which the frame is read.
	run := func(name string, readAfter ...int) {
		h264Format := &format.H264{PayloadTyp: 96, PacketizationMode: 1}
		rtpDec, err := h264Format.CreateDecoder()
		if err != nil {
			t.Fatalf("CreateDecoder: %v", err)
		}
		sc := &SnapshotConsumer{
			camera:      "test",
			frameDec:    NewH264Decoder(),
			newDecoder:  func() FrameDecoder { return NewH264Decoder() },
			h264Decoder: rtsp.NewH264AccessUnitDecoder(rtpDec),
		}
		defer sc.Close()
		packetizer := rtph264.Encoder{PayloadType: 96, PacketizationMode: 1}
		if err := packetizer.Init(); err != nil {
			t.Fatal(err)
		}
		keyframe := -1
		for i, unit := range units {
			packets, err := packetizer.Encode(unit)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range packets {
				p.Timestamp = uint32(i * 9000)
				sc.OnVideoRTP(p)
			}
			sc.mu.Lock()
			arrived := int(sc.seq) - 1
			sc.mu.Unlock()
			if arrived == keyframe {
				continue
			}
			keyframe = arrived
			for _, at := range readAfter {
				if at != keyframe {
					continue
				}
				frame := sc.LastFrame()
				if frame == nil {
					t.Fatalf("%s: no frame after keyframe %d", name, keyframe)
				}
				if !bytes.Equal(frame.Pix, shows[keyframe]) {
					t.Fatalf("%s: the frame read after keyframe %d shows another picture", name, keyframe)
				}
			}
		}
		if keyframe != len(keyframes)-1 {
			t.Fatalf("%s: the consumer saw %d keyframes, want %d", name, keyframe+1, len(keyframes))
		}
	}
	run("every keyframe read", 0, 1, 2)
	run("IDR unread", 2)
	run("IDR read, then a gap", 0, 2)
}
