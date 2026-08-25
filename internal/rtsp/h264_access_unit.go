package rtsp

import (
	"errors"

	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/pion/rtp"
)

// H264AccessUnitDecoder assembles H.264 access units by RTP timestamp instead
// of trusting the RTP marker bit. A number of cameras set Marker on every NAL
// unit (including SEI and individual slices), even though RFC 6184 reserves it
// for the final packet of a complete access unit. Passing those packets to the
// stock decoder unchanged produces partial frames, corrupt MP4 samples, and
// visible decoder artifacts.
//
// Timestamp boundaries are authoritative for H.264 RTP and are also supported
// by the underlying decoder. Clearing Marker adds at most one frame of latency
// while making both compliant and non-compliant camera streams deterministic.
type H264AccessUnitDecoder struct {
	decoder          *rtph264.Decoder
	haveTimestamp    bool
	currentTimestamp uint32
}

// NewH264AccessUnitDecoder wraps a configured RTP/H.264 decoder.
func NewH264AccessUnitDecoder(decoder *rtph264.Decoder) *H264AccessUnitDecoder {
	return &H264AccessUnitDecoder{decoder: decoder}
}

// Decode returns a complete access unit and its original RTP timestamp.
func (d *H264AccessUnitDecoder) Decode(pkt *rtp.Packet) ([][]byte, uint32, error) {
	if d == nil || d.decoder == nil {
		return nil, 0, errors.New("H264 access-unit decoder is not initialized")
	}
	if pkt == nil {
		return nil, 0, errors.New("nil H264 RTP packet")
	}

	if !d.haveTimestamp {
		d.currentTimestamp = pkt.Timestamp
		d.haveTimestamp = true
	}

	accessUnitTimestamp := d.currentTimestamp
	timestampChanged := pkt.Timestamp != d.currentTimestamp

	// The payload is immutable and can remain shared; only Marker is
	// normalized for the depacketizer.
	normalized := *pkt
	normalized.Marker = false
	au, err := d.decoder.Decode(&normalized)
	if len(au) > 0 && timestampChanged {
		d.currentTimestamp = pkt.Timestamp
	}
	return au, accessUnitTimestamp, err
}

// Flush returns the final buffered access unit. It is intended for finite
// writers that must not lose the last frame when a segment closes.
func (d *H264AccessUnitDecoder) Flush() ([][]byte, uint32, error) {
	if d == nil || d.decoder == nil || !d.haveTimestamp {
		return nil, 0, nil
	}

	// An Access Unit Delimiter at the next timestamp forces the underlying
	// timestamp-based assembler to emit its current buffer. The synthetic AUD
	// remains buffered inside a decoder that is about to be discarded.
	pkt := &rtp.Packet{
		Header:  rtp.Header{Timestamp: d.currentTimestamp + 1},
		Payload: []byte{0x09, 0xf0},
	}
	return d.Decode(pkt)
}
