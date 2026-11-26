package media

import (
	"image"
	"log/slog"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediacommon/v2/pkg/bits"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/pion/rtp"

	"github.com/rvben/vedetta/internal/rtsp"
)

// SnapshotConsumer keeps the latest random-access unit of the main (high-res)
// stream and decodes it when something asks for the frame.
//
// Snapshots are read on events, far less often than a keyframe arrives, and
// decoding and converting a full-resolution frame is the expensive part: a
// keyframe nobody asks for costs one copy of its bytes. The price is paid by
// the first reader of each keyframe, which waits for one decode.
//
// That holds for a unit with an IDR picture, which decodes on its own. A unit
// that is random access through a recovery point decodes only on a decoder
// that has decoded every random-access unit since the last IDR, in order, so
// those are decoded as they arrive, on a goroutine of their own.
type SnapshotConsumer struct {
	camera string

	// h264Decoder, seq, sps and pps belong to the RTP fan-out goroutine.
	h264Decoder *rtsp.H264AccessUnitDecoder
	// sps and pps hold the latest parameter set seen for each ID. A slice may
	// refer to a set that arrived with a keyframe nobody decoded.
	sps [maxSPSID + 1][]byte
	pps [maxPPSID + 1][]byte
	// seq numbers the random-access units in arrival order, from 1, the ones
	// that are not kept included.
	seq uint64

	// mu guards the undecoded units. The RTP fan-out goroutine takes it for
	// every keyframe, so nothing slow ever runs under it.
	mu sync.Mutex
	// unit is the latest unit with an IDR picture, empty once a reader took it.
	unit keyUnit
	// older is the unread unit that unit replaced, decoded when unit turns
	// out to be damaged.
	older keyUnit
	// chain holds the units to decode in arrival order: recovery-point units,
	// each run headed by the IDR unit before it when nobody had read that.
	// Every unit in it is older than unit.
	chain []keyUnit
	// draining says a goroutine is on its way to decode chain.
	draining bool

	// decodeMu serialises readers: the decoder and everything below belong to
	// whoever holds it.
	decodeMu sync.Mutex
	frameDec FrameDecoder
	// newDecoder builds the replacement for a decoder that failed on a unit.
	newDecoder func() FrameDecoder
	// decodedSeq is the number of the unit frameDec decoded last, 0 when it
	// has decoded none.
	decodedSeq uint64
	closed     bool
	// picture is the buffer a reusing decoder decodes into. It is converted
	// before decodeMu is released, so no reader ever sees it half written.
	picture *image.YCbCr
	// lastFrame is the latest decoded frame and lastTime when its unit
	// arrived. A frame is never written again once handed out.
	lastFrame *image.RGBA
	lastTime  time.Time

	closeOnce sync.Once
}

// NewSnapshotConsumer creates a consumer that serves the latest full-resolution
// frame of the main stream. Returns nil if the track is not H264 or no H264
// decoder is available.
func NewSnapshotConsumer(camera string, track *rtsp.TrackInfo) *SnapshotConsumer {
	if track == nil || track.Codec != "H264" {
		return nil
	}

	// The track's parameter sets are read once: the source may fill them in
	// later from another goroutine, and every unit carries its own anyway.
	sps, pps := track.SPS, track.PPS

	// Build the frame decoder up front so the consumer is only created when
	// decode is actually possible. Honors the configured hwaccel preference and
	// falls back to software; on hardware-only systems this succeeds even when
	// OpenH264 is absent.
	frameDec := NewDefaultFrameDecoder(sps, pps)
	if frameDec == nil {
		slog.Warn("snapshot consumer: no H264 decoder available", "camera", camera)
		return nil
	}

	h264Format := &format.H264{
		PayloadTyp:        96,
		PacketizationMode: 1,
		SPS:               sps,
		PPS:               pps,
	}
	dec, err := h264Format.CreateDecoder()
	if err != nil {
		frameDec.Close()
		slog.Warn("snapshot consumer: failed to create H264 RTP decoder", "camera", camera, "error", err)
		return nil
	}

	sc := &SnapshotConsumer{
		camera:      camera,
		h264Decoder: rtsp.NewH264AccessUnitDecoder(dec),
		frameDec:    frameDec,
		newDecoder:  func() FrameDecoder { return NewDefaultFrameDecoder(sps, pps) },
	}
	sc.rememberParameterSet(sps)
	sc.rememberParameterSet(pps)

	slog.Info("snapshot consumer enabled for main stream", "camera", camera)
	return sc
}

// LastFrame returns the latest full-resolution frame, or nil.
func (sc *SnapshotConsumer) LastFrame() *image.RGBA {
	frame, _ := sc.LastFrameAt()
	return frame
}

// LastFrameAt returns the latest full-resolution frame with the time its
// keyframe arrived. The first call after a new keyframe decodes it; later calls
// share the result. When that unit does not decode, the unread one before it
// is tried, and failing that the previous frame stays in place.
func (sc *SnapshotConsumer) LastFrameAt() (*image.RGBA, time.Time) {
	sc.decodeMu.Lock()
	defer sc.decodeMu.Unlock()

	sc.mu.Lock()
	chain, unit, older := sc.chain, sc.unit, sc.older
	sc.chain, sc.unit, sc.older = nil, keyUnit{}, keyUnit{}
	sc.mu.Unlock()

	for _, u := range chain {
		sc.decodeLocked(u)
	}
	if !sc.decodeLocked(unit) {
		sc.decodeLocked(older)
	}
	return sc.lastFrame, sc.lastTime
}

// drainChain decodes the units that have to be decoded in arrival order.
func (sc *SnapshotConsumer) drainChain() {
	sc.decodeMu.Lock()
	defer sc.decodeMu.Unlock()

	sc.mu.Lock()
	chain := sc.chain
	sc.chain, sc.draining = nil, false
	sc.mu.Unlock()

	for _, u := range chain {
		sc.decodeLocked(u)
	}
}

// decodeLocked makes u the latest frame and reports whether it decoded. A unit
// that follows on from the one before it is left alone unless the decoder
// decoded that one last. A decoder that failed or had to be drained is
// replaced. The caller holds decodeMu.
func (sc *SnapshotConsumer) decodeLocked(u keyUnit) bool {
	if u.data == nil || sc.closed {
		return false
	}
	if u.followsOn && (sc.decodedSeq == 0 || sc.decodedSeq != u.seq-1) {
		return false
	}
	if sc.frameDec == nil {
		// The last replacement could not be built: try again for this unit.
		if sc.newDecoder == nil {
			return false
		}
		if sc.frameDec = sc.newDecoder(); sc.frameDec == nil {
			return false
		}
	}
	picture, spent := decodeKeyframe(sc.frameDec, u.data, sc.picture)
	if picture == nil {
		sc.replaceDecoderLocked()
		return false
	}
	if _, reuses := sc.frameDec.(reusingFrameDecoder); reuses {
		sc.picture = picture
	}
	sc.lastFrame = ycbcrToRGBA(picture)
	sc.lastTime = u.at
	sc.decodedSeq = u.seq
	if spent {
		sc.replaceDecoderLocked()
	}
	return true
}

// replaceDecoderLocked swaps in a new decoder after a unit failed to decode or
// the decoder had to be drained for its picture. A decoder that rejected a
// damaged unit can reject the intact one that follows, and a drained one
// accepts no more input. The new decoder has decoded nothing, so the units
// that follow on from an earlier one wait for the next IDR. The caller holds
// decodeMu.
func (sc *SnapshotConsumer) replaceDecoderLocked() {
	if sc.newDecoder == nil {
		return
	}
	sc.frameDec.Close()
	sc.frameDec = sc.newDecoder()
	sc.decodedSeq = 0
}

// Close releases decoder resources, waiting for a decode in progress. The last
// frame stays readable. Safe to call more than once.
func (sc *SnapshotConsumer) Close() {
	sc.closeOnce.Do(func() {
		sc.decodeMu.Lock()
		defer sc.decodeMu.Unlock()
		sc.closed = true
		if sc.frameDec != nil {
			sc.frameDec.Close()
		}
		// Nothing can decode the pending units any more.
		sc.mu.Lock()
		sc.chain, sc.unit, sc.older = nil, keyUnit{}, keyUnit{}
		sc.mu.Unlock()
	})
}

// OnVideoRTP reassembles NAL units and keeps the random-access units.
func (sc *SnapshotConsumer) OnVideoRTP(pkt *rtp.Packet) {
	if sc.h264Decoder == nil {
		return
	}

	au, _, err := sc.h264Decoder.Decode(pkt)
	if err != nil {
		return
	}

	if !h264.IsRandomAccess(au) {
		return
	}
	sc.seq++

	// Learn the parameter sets this unit brings, and find its first slice.
	var slice []byte
	hasIDR := false
	size := 0
	for _, nalu := range au {
		if len(nalu) == 0 {
			continue
		}
		switch h264.NALUType(nalu[0] & 0x1F) {
		case h264.NALUTypeSPS, h264.NALUTypePPS:
			sc.rememberParameterSet(nalu)
			continue
		case h264.NALUTypeIDR:
			hasIDR = true
			if slice == nil {
				slice = nalu
			}
		case h264.NALUTypeNonIDR:
			// A unit is also random access through a recovery point, without an IDR.
			if slice == nil {
				slice = nalu
			}
		}
		size += 4 + len(nalu)
	}

	// The unit may be the first one its decoder sees, so it starts with the
	// PPS its first slice refers to and the SPS that PPS refers to, in the order
	// a decoder needs them. No other set goes in: a decoder that holds a single
	// pair takes the last one it is given. A unit whose sets are unknown cannot
	// be decoded and is not kept.
	ppsID, ok := sliceParameterSetID(slice)
	if !ok || sc.pps[ppsID] == nil {
		return
	}
	pps := sc.pps[ppsID]
	spsID, ok := ppsSequenceID(pps)
	if !ok || sc.sps[spsID] == nil {
		return
	}
	sps := sc.sps[spsID]

	unit := make([]byte, 0, size+8+len(sps)+len(pps))
	appendNALU := func(nalu []byte) {
		unit = append(unit, 0, 0, 0, 1)
		unit = append(unit, nalu...)
	}
	appendNALU(sps)
	appendNALU(pps)
	for _, nalu := range au {
		if len(nalu) == 0 {
			continue
		}
		switch h264.NALUType(nalu[0] & 0x1F) {
		case h264.NALUTypeSPS, h264.NALUTypePPS:
		default:
			appendNALU(nalu)
		}
	}

	now := time.Now()
	sc.mu.Lock()
	arrived := keyUnit{data: unit, at: now, seq: sc.seq, followsOn: !hasIDR}
	if hasIDR {
		sc.older, sc.unit = sc.unit, arrived
		sc.mu.Unlock()
		return
	}
	// An unread unit here is the IDR this one follows on from.
	if sc.unit.data != nil {
		sc.chain = append(sc.chain, sc.unit)
	}
	sc.unit, sc.older = keyUnit{}, keyUnit{}
	if len(sc.chain) >= maxChainedUnits {
		// The decoder is not keeping up. Without these units the ones after
		// them cannot be decoded either, until the next IDR.
		sc.chain = nil
	}
	sc.chain = append(sc.chain, arrived)
	drain := !sc.draining
	if drain {
		sc.draining = true
	}
	sc.mu.Unlock()
	if drain {
		go sc.drainChain()
	}
}

// keyUnit is a random-access unit as an Annex B stream.
type keyUnit struct {
	data []byte
	at   time.Time
	seq  uint64
	// followsOn says the unit is random access through a recovery point and
	// decodes only after the unit before it.
	followsOn bool
}

// maxChainedUnits bounds the units waiting to be decoded in order.
const maxChainedUnits = 8

// The largest seq_parameter_set_id and pic_parameter_set_id H.264 allows.
const (
	maxSPSID = 31
	maxPPSID = 255
)

// rememberParameterSet stores an SPS or PPS under its ID and returns that ID.
// A set whose ID cannot be read is not stored and reports false; any other
// NAL unit type is ignored the same way.
func (sc *SnapshotConsumer) rememberParameterSet(nalu []byte) (id uint32, ok bool) {
	if len(nalu) == 0 {
		return 0, false
	}
	switch h264.NALUType(nalu[0] & 0x1F) {
	case h264.NALUTypeSPS:
		// profile_idc, the constraint flags and level_idc precede the ID.
		if id, ok = parameterSetID(nalu, 24, maxSPSID); ok {
			sc.sps[id] = nalu
		}
	case h264.NALUTypePPS:
		if id, ok = parameterSetID(nalu, 0, maxPPSID); ok {
			sc.pps[id] = nalu
		}
	}
	return id, ok
}

// parameterSetID reads the Exp-Golomb ID that starts bit bits into the payload
// of a parameter set NAL unit. It reports false when the unit is too short or
// the ID exceeds limit.
func parameterSetID(nalu []byte, bit int, limit uint32) (uint32, bool) {
	ids, ok := readGolombs(nalu, bit, 1)
	if !ok || ids[0] > limit {
		return 0, false
	}
	return ids[0], true
}

// ppsSequenceID returns the seq_parameter_set_id a PPS refers to, the field
// after its own ID.
func ppsSequenceID(pps []byte) (uint32, bool) {
	ids, ok := readGolombs(pps, 0, 2)
	if !ok || ids[1] > maxSPSID {
		return 0, false
	}
	return ids[1], true
}

// sliceParameterSetID returns the pic_parameter_set_id of a slice NAL unit,
// the field after first_mb_in_slice and slice_type.
func sliceParameterSetID(slice []byte) (uint32, bool) {
	ids, ok := readGolombs(slice, 0, 3)
	if !ok || ids[2] > maxPPSID {
		return 0, false
	}
	return ids[2], true
}

// golombPrefix is how much of a NAL unit readGolombs looks at. Three Exp-Golomb
// values of 32 bits each take at most 195 bits, and emulation prevention adds
// at most a byte for every two.
const golombPrefix = 40

// readGolombs reads n consecutive Exp-Golomb values (n at most 3) starting bit
// bits into the payload of a NAL unit. It reports false when the unit ends
// first.
func readGolombs(nalu []byte, bit, n int) (values [3]uint32, ok bool) {
	if len(nalu) < 2 {
		return values, false
	}
	payload := h264.EmulationPreventionRemove(nalu[1:min(len(nalu), 1+golombPrefix)])
	for i := range n {
		// ReadGolombUnsigned checks for the exact end of the payload only: a
		// start beyond it reads out of bounds.
		if len(payload)*8 <= bit {
			return values, false
		}
		v, err := bits.ReadGolombUnsigned(payload, &bit)
		if err != nil {
			return values, false
		}
		values[i] = v
	}
	return values, true
}

// OnAudioRTP is a no-op.
func (sc *SnapshotConsumer) OnAudioRTP(_ *rtp.Packet) {}

// OnDisconnect is called when the source disconnects.
func (sc *SnapshotConsumer) OnDisconnect() {}

// ycbcrToRGBA converts a YCbCr image to RGBA at native resolution.
func ycbcrToRGBA(img *image.YCbCr) *image.RGBA {
	bounds := img.Rect
	w := bounds.Dx()
	h := bounds.Dy()
	rgba := image.NewRGBA(image.Rect(0, 0, w, h))

	for dy := 0; dy < h; dy++ {
		for dx := 0; dx < w; dx++ {
			sy := dy + bounds.Min.Y
			sx := dx + bounds.Min.X

			yi := sy*img.YStride + sx
			ci := (sy/2)*img.CStride + (sx / 2)

			yy := int(img.Y[yi])
			cbb := int(img.Cb[ci]) - 128
			crr := int(img.Cr[ci]) - 128

			r := yy + ((91881*crr + 32768) >> 16)
			g := yy - ((22554*cbb + 46802*crr + 32768) >> 16)
			b := yy + ((116130*cbb + 32768) >> 16)

			if r < 0 {
				r = 0
			} else if r > 255 {
				r = 255
			}
			if g < 0 {
				g = 0
			} else if g > 255 {
				g = 255
			}
			if b < 0 {
				b = 0
			} else if b > 255 {
				b = 255
			}

			off := dy*rgba.Stride + dx*4
			rgba.Pix[off] = byte(r)
			rgba.Pix[off+1] = byte(g)
			rgba.Pix[off+2] = byte(b)
			rgba.Pix[off+3] = 255
		}
	}

	return rgba
}
