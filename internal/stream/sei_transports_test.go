package stream

import (
	"bytes"
	"encoding/binary"
	"log/slog"
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/pion/rtp"

	"github.com/rvben/vedetta/internal/rtsp"
)

// A real High-profile SPS/PPS pair, so what the consumers build from it is
// well-formed rather than accepted by accident.
var (
	seiTestSPS = []byte{
		0x67, 0x64, 0x00, 0x28, 0xac, 0xd9, 0x40, 0x78, 0x02, 0x27, 0xe5, 0x84,
		0x00, 0x00, 0x03, 0x00, 0x04, 0x00, 0x00, 0x03, 0x00, 0xf0, 0x3c, 0x60, 0xc6, 0x58,
	}
	seiTestPPS = []byte{0x68, 0xee, 0x3c, 0x80}
	seiTestIDR = []byte{0x65, 0x88, 0x84, 0x00, 0x33, 0xff}
	seiTestP   = []byte{0x41, 0x9a, 0x21, 0x6c, 0x45, 0xff}
)

// seiNAL is a user_data_unregistered SEI shaped like the one the TP-Link C220
// injects: NAL type 6, payload type 5, a 16-byte UUID and a marker string.
func seiNAL() []byte {
	nal := []byte{0x06, 0x05, 0x18}
	nal = append(nal, bytes.Repeat([]byte{0xAB}, 16)...)
	nal = append(nal, []byte("TPLINKMARKERBOX")...)
	return append(nal, 0x80)
}

func isSEI(nalu []byte) bool {
	return len(nalu) > 0 && h264.NALUType(nalu[0]&0x1F) == h264.NALUTypeSEI
}

// avccContainsSEI walks a length-prefixed AVCC sample, the form the fMP4
// muxers store, and reports whether any NAL in it is SEI.
func avccContainsSEI(t *testing.T, avcc []byte) bool {
	t.Helper()
	for i := 0; i+4 <= len(avcc); {
		size := int(binary.BigEndian.Uint32(avcc[i : i+4]))
		i += 4
		if size <= 0 || i+size > len(avcc) {
			t.Fatalf("malformed AVCC sample: length %d at offset %d exceeds %d bytes", size, i, len(avcc))
		}
		if isSEI(avcc[i:]) {
			return true
		}
		i += size
	}
	return false
}

// seiTestEncoder packetizes access units the way a camera does, so the consumer
// under test runs its real depacketize path rather than being handed an AU.
func seiTestEncoder(t *testing.T) *rtph264.Encoder {
	t.Helper()
	f := &format.H264{PayloadTyp: 96, PacketizationMode: 1, SPS: seiTestSPS, PPS: seiTestPPS}
	enc, err := f.CreateEncoder()
	if err != nil {
		t.Fatalf("create H264 RTP encoder: %v", err)
	}
	return enc
}

func encodeAU(t *testing.T, enc *rtph264.Encoder, au [][]byte, ts uint32) []*rtp.Packet {
	t.Helper()
	pkts, err := enc.Encode(au)
	if err != nil {
		t.Fatalf("encode access unit: %v", err)
	}
	for _, p := range pkts {
		p.Timestamp = ts
	}
	return pkts
}

// The MSE fragments a browser receives must carry no SEI. Chrome tolerates the
// junk SEI that breaks iOS, so a regression here is invisible on the desktop
// dashboard and surfaces only as the frame dropping this strip was added to fix.
func TestMSEConsumer_StripsSEIFromFragments(t *testing.T) {
	slog.SetDefault(slog.New(slog.DiscardHandler))

	mc := newMSEConsumer("cam", &rtsp.TrackInfo{Codec: "H264", SPS: seiTestSPS, PPS: seiTestPPS}, nil)
	if mc.h264Decoder == nil {
		t.Fatal("MSE consumer has no H264 decoder, so no packet would be depacketized")
	}
	enc := seiTestEncoder(t)

	// Two things buffer here. The access-unit decoder closes a unit only when a
	// packet with a later timestamp arrives, and the sample timer then holds the
	// newest unit and hands back the previous one finalized. Three access units
	// is the smallest sequence that puts a sample in the fragment queue.
	aus := [][][]byte{
		{seiTestSPS, seiTestPPS, seiTestIDR, seiNAL()},
		{seiNAL(), seiTestP},
		{seiNAL(), seiTestP},
	}
	for i, au := range aus {
		for _, pkt := range encodeAU(t, enc, au, uint32(90000+3000*i)) {
			mc.OnVideoRTP(pkt)
		}
	}

	mc.mu.Lock()
	samples := mc.pendingVideo
	mc.mu.Unlock()

	if len(samples) == 0 {
		t.Fatal("no video sample reached the fragment queue, so the assertion below would pass vacuously")
	}
	for i, s := range samples {
		if avccContainsSEI(t, s.Payload) {
			t.Errorf("MSE sample %d still carries an SEI NAL", i)
		}
	}
}

// recordingStreamWriter captures the packets the republisher publishes.
type recordingStreamWriter struct {
	packets []*rtp.Packet
}

func (w *recordingStreamWriter) WritePacketRTP(_ *description.Media, pkt *rtp.Packet) error {
	clone := *pkt
	clone.Payload = append([]byte(nil), pkt.Payload...)
	w.packets = append(w.packets, &clone)
	return nil
}

// The republished RTSP stream feeds clients as strict as iOS, so it has to be
// normalized the same way every other transport is.
func TestRTSPServerConsumer_StripsSEIFromRepublishedPackets(t *testing.T) {
	slog.SetDefault(slog.New(slog.DiscardHandler))

	h264Fmt := &format.H264{PayloadTyp: 96, PacketizationMode: 1, SPS: seiTestSPS, PPS: seiTestPPS}
	video := &description.Media{Type: description.MediaTypeVideo, Formats: []format.Format{h264Fmt}}

	dec, err := h264Fmt.CreateDecoder()
	if err != nil {
		t.Fatalf("create decoder: %v", err)
	}
	outEnc, err := h264Fmt.CreateEncoder()
	if err != nil {
		t.Fatalf("create encoder: %v", err)
	}

	writer := &recordingStreamWriter{}
	c := &rtspServerConsumer{
		stream:     writer,
		videoMedia: video,
		videoPT:    h264Fmt.PayloadType(),
		h264Format: h264Fmt,
		rtpDecoder: rtsp.NewH264AccessUnitDecoder(dec),
		rtpEncoder: outEnc,
	}

	enc := seiTestEncoder(t)
	// The access-unit decoder closes a unit when a packet with a later timestamp
	// arrives, so a second access unit is needed to publish the first.
	for i, au := range [][][]byte{
		{seiTestSPS, seiTestPPS, seiTestIDR, seiNAL()},
		{seiNAL(), seiTestP},
	} {
		for _, pkt := range encodeAU(t, enc, au, uint32(90000+3000*i)) {
			c.OnVideoRTP(pkt)
		}
	}
	if len(writer.packets) == 0 {
		t.Fatal("nothing was republished, so the assertion below would pass vacuously")
	}

	// Depacketize what was published and inspect the access unit a client sees.
	verify, err := h264Fmt.CreateDecoder()
	if err != nil {
		t.Fatalf("create verification decoder: %v", err)
	}
	var published [][]byte
	for _, pkt := range writer.packets {
		au, derr := verify.Decode(pkt)
		if derr != nil {
			continue
		}
		published = append(published, au...)
	}
	if len(published) == 0 {
		t.Fatal("the republished packets did not depacketize into an access unit")
	}
	for _, nalu := range published {
		if isSEI(nalu) {
			t.Error("republished access unit still carries an SEI NAL")
		}
	}
}

// Native HLS is the transport the SEI strip was added for: the TP-Link SEI made
// iOS VideoToolbox reject nearly every frame, collapsing playback to a
// keyframe-only slideshow. The stripped bitstream has to reach the segment.
func TestHLSConsumer_StripsSEIFromSegmentSamples(t *testing.T) {
	slog.SetDefault(slog.New(slog.DiscardHandler))

	c := newHLSConsumer(&rtsp.TrackInfo{Codec: "H264", SPS: seiTestSPS, PPS: seiTestPPS}, nil)
	if c.h264Decoder == nil {
		t.Fatal("HLS consumer has no H264 decoder, so no packet would be depacketized")
	}
	enc := seiTestEncoder(t)

	aus := [][][]byte{
		{seiTestSPS, seiTestPPS, seiTestIDR, seiNAL()},
		{seiNAL(), seiTestP},
		{seiNAL(), seiTestP},
	}
	for i, au := range aus {
		for _, pkt := range encodeAU(t, enc, au, uint32(90000+3000*i)) {
			c.OnVideoRTP(pkt)
		}
	}

	c.mu.Lock()
	samples := c.segVideo
	c.mu.Unlock()

	if len(samples) == 0 {
		t.Fatal("no video sample reached the segment, so the assertion below would pass vacuously")
	}
	for i, s := range samples {
		if avccContainsSEI(t, s.Payload) {
			t.Errorf("HLS segment sample %d still carries an SEI NAL", i)
		}
	}
}
