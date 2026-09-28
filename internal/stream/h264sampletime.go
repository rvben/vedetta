package stream

import (
	"log/slog"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
)

// h264SampleTimer turns a decode-order stream of H.264 access units into
// fMP4 samples with correct decode timing, shared by the HLS and MSE muxers.
//
// RTP timestamps are presentation times (PTS) in 90 kHz ticks. Streams with
// B-frames (typically High profile) reorder frames, so PTS != DTS: the fMP4
// sample must carry PTS-DTS as its composition-time offset and its duration
// must be the DECODE-order (DTS) delta. Stamping a zero offset and a
// PTS-delta duration - correct only for B-frame-free streams - makes a real
// AVPlayer present frames out of order and effectively render only the
// IDR/anchor frames, which looks like one new still image per segment.
//
// A sample's duration is only known once its successor arrives (duration =
// DTS[next] - DTS[current]), so the most recent sample is held in flight
// until the next push. The zero value is not usable; call newH264SampleTimer.
type h264SampleTimer struct {
	label string

	// Out-of-band SPS/PPS from the SDP. Many cameras never repeat the SPS
	// in-band; the DTS extractor learns reordering depth only from an
	// in-band SPS, so keyframes that lack one are prefixed with these.
	sdpSPS []byte
	sdpPPS []byte

	dts *h264.DTSExtractor

	// RTP timestamps are uint32 and wrap (~13.25 h at 90 kHz); the DTS
	// extractor needs a monotonic int64 PTS.
	unwrapInit  bool
	unwrapLast  uint32
	unwrapAccum int64

	inFlight    *fmp4.Sample
	inFlightDTS int64
	hasInFlight bool

	fallbackLogged bool
}

func newH264SampleTimer(label string) *h264SampleTimer {
	t := &h264SampleTimer{label: label}
	t.reset()
	return t
}

// setParameterSets records the SDP (out-of-band) SPS/PPS. When a
// random-access AU arrives without an in-band SPS, these are prefixed to it
// so the DTS extractor can initialize and the emitted keyframe sample is
// self-contained. Safe to call before or after the timer starts.
func (t *h264SampleTimer) setParameterSets(sps, pps []byte) {
	t.sdpSPS = sps
	t.sdpPPS = pps
}

// reset re-establishes the timer after an SPS change or discontinuity. The
// next access unit must be a keyframe; callers already gate on that before
// the first push of a new epoch.
func (t *h264SampleTimer) reset() {
	d := &h264.DTSExtractor{}
	d.Initialize()
	t.dts = d
	t.unwrapInit = false
	t.unwrapLast = 0
	t.unwrapAccum = 0
	t.inFlight = nil
	t.inFlightDTS = 0
	t.hasInFlight = false
	t.fallbackLogged = false
}

// push consumes the next access unit in decode order; rtpTS is its RTP
// (presentation) timestamp. It returns the now-finalized previous sample,
// ready to append to the current fragment, and that sample's decode-order
// duration in 90 kHz ticks. ok is false on the first push of an epoch (no
// prior sample) or when the access unit cannot be marshalled.
func (t *h264SampleTimer) push(au [][]byte, rtpTS uint32) (finalized *fmp4.Sample, durTicks uint32, ok bool) {
	pts := t.unwrapPTS(rtpTS)
	au = t.withParameterSets(au)

	dts, err := t.dts.Extract(au, pts)
	if err != nil {
		// A malformed AU or a post-discontinuity gap can break DTS
		// recovery. Fall back to PTS (exactly correct for B-frame-free
		// streams, and the next keyframe/SPS change re-establishes the
		// extractor) instead of dropping the stream.
		dts = pts
		if !t.fallbackLogged {
			slog.Warn("H.264 DTS extraction failed; using PTS until next keyframe",
				"stream", t.label, "error", err)
			t.fallbackLogged = true
		}
	}

	avcc, err := h264.AVCC(au).Marshal()
	if err != nil {
		return nil, 0, false
	}
	next := &fmp4.Sample{
		// PTSOffset is the composition time (PTS-DTS): zero for B-frame-free
		// streams, non-zero wherever the encoder reordered frames.
		PTSOffset:       int32(pts - dts),
		IsNonSyncSample: !h264.IsRandomAccess(au),
		Payload:         avcc,
	}

	if t.hasInFlight {
		delta := dts - t.inFlightDTS
		// DTS is monotonic in decode order, so a non-positive or absurd
		// gap means a glitch/discontinuity; fall back to a 30fps frame.
		if delta <= 0 || delta > 90000*2 {
			delta = 90000 / 30
		}
		t.inFlight.Duration = uint32(delta)
		finalized = t.inFlight
		durTicks = uint32(delta)
		ok = true
	}

	t.inFlight = next
	t.inFlightDTS = dts
	t.hasInFlight = true
	return finalized, durTicks, ok
}

// withParameterSets prefixes the SDP SPS/PPS to a random-access AU that
// lacks an in-band SPS, so the DTS extractor can initialize and the emitted
// keyframe sample is self-contained. Non-keyframe AUs and AUs that already
// carry an SPS are returned unchanged.
func (t *h264SampleTimer) withParameterSets(au [][]byte) [][]byte {
	if len(t.sdpSPS) == 0 || !h264.IsRandomAccess(au) {
		return au
	}
	for _, nalu := range au {
		if len(nalu) > 0 && h264.NALUType(nalu[0]&0x1F) == h264.NALUTypeSPS {
			return au
		}
	}
	out := make([][]byte, 0, len(au)+2)
	out = append(out, t.sdpSPS)
	if len(t.sdpPPS) > 0 {
		out = append(out, t.sdpPPS)
	}
	return append(out, au...)
}

func (t *h264SampleTimer) unwrapPTS(rtpTS uint32) int64 {
	if !t.unwrapInit {
		t.unwrapInit = true
		t.unwrapLast = rtpTS
		t.unwrapAccum = int64(rtpTS)
		return t.unwrapAccum
	}
	// Signed delta absorbs the uint32 wrap and minor reordering.
	t.unwrapAccum += int64(int32(rtpTS - t.unwrapLast))
	t.unwrapLast = rtpTS
	return t.unwrapAccum
}
