package rtsp

import (
	"errors"
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/pion/rtp"
)

func newAccessUnitTestDecoder(t *testing.T) *H264AccessUnitDecoder {
	t.Helper()
	inner := &rtph264.Decoder{PacketizationMode: 1}
	if err := inner.Init(); err != nil {
		t.Fatalf("init RTP/H264 decoder: %v", err)
	}
	return NewH264AccessUnitDecoder(inner)
}

func TestH264AccessUnitDecoderIgnoresSpuriousMarkerBits(t *testing.T) {
	dec := newAccessUnitTestDecoder(t)

	packets := []*rtp.Packet{
		{Header: rtp.Header{SequenceNumber: 1, Timestamp: 9000, Marker: true}, Payload: []byte{0x06, 0xaa}},
		{Header: rtp.Header{SequenceNumber: 2, Timestamp: 9000, Marker: true}, Payload: []byte{0x65, 0xbb}},
		{Header: rtp.Header{SequenceNumber: 3, Timestamp: 12000, Marker: true}, Payload: []byte{0x41, 0xcc}},
	}

	for i := 0; i < 2; i++ {
		au, _, err := dec.Decode(packets[i])
		if !errors.Is(err, rtph264.ErrMorePacketsNeeded) {
			t.Fatalf("packet %d error = %v, want ErrMorePacketsNeeded", i, err)
		}
		if au != nil {
			t.Fatalf("packet %d emitted a partial access unit: %v", i, au)
		}
	}

	au, timestamp, err := dec.Decode(packets[2])
	if err != nil {
		t.Fatalf("timestamp boundary decode: %v", err)
	}
	if timestamp != 9000 {
		t.Fatalf("access-unit timestamp = %d, want 9000", timestamp)
	}
	if len(au) != 2 || au[0][0] != 0x06 || au[1][0] != 0x65 {
		t.Fatalf("access unit = %v, want combined SEI and IDR NALs", au)
	}
}

func TestH264AccessUnitDecoderFlushesFinalFrame(t *testing.T) {
	dec := newAccessUnitTestDecoder(t)
	pkt := &rtp.Packet{
		Header:  rtp.Header{SequenceNumber: 1, Timestamp: 4500, Marker: true},
		Payload: []byte{0x65, 0xdd},
	}

	if au, _, err := dec.Decode(pkt); !errors.Is(err, rtph264.ErrMorePacketsNeeded) || au != nil {
		t.Fatalf("initial decode = (%v, %v), want buffered frame", au, err)
	}

	au, timestamp, err := dec.Flush()
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if timestamp != 4500 {
		t.Fatalf("flushed timestamp = %d, want 4500", timestamp)
	}
	if len(au) != 1 || au[0][0] != 0x65 {
		t.Fatalf("flushed access unit = %v, want IDR", au)
	}
}
