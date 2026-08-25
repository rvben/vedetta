package media

import (
	"image"
	"log/slog"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/pion/rtp"

	"github.com/rvben/vedetta/internal/rtsp"
)

// SnapshotConsumer decodes IDR frames from the main (high-res) stream
// and caches the latest decoded frame for event snapshots.
// Decoding runs in a dedicated goroutine to avoid blocking RTP fan-out.
type SnapshotConsumer struct {
	camera string

	h264Decoder *rtsp.H264AccessUnitDecoder
	frameDec    FrameDecoder
	sps         []byte
	pps         []byte

	mu        sync.RWMutex
	lastFrame *image.RGBA
	lastTime  time.Time

	decodeCh  chan []byte
	done      chan struct{}
	closeOnce sync.Once
}

// NewSnapshotConsumer creates a consumer that caches the latest full-resolution
// decoded frame from the main stream. Returns nil if the track is not H264 or
// no H264 decoder is available.
func NewSnapshotConsumer(camera string, track *rtsp.TrackInfo) *SnapshotConsumer {
	if track == nil || track.Codec != "H264" {
		return nil
	}

	// Build the frame decoder up front so the consumer is only created when
	// decode is actually possible. Honors the configured hwaccel preference and
	// falls back to software; on hardware-only systems this succeeds even when
	// OpenH264 is absent. Owned and closed by decodeLoop.
	frameDec := NewDefaultFrameDecoder(track.SPS, track.PPS)
	if frameDec == nil {
		slog.Warn("snapshot consumer: no H264 decoder available", "camera", camera)
		return nil
	}

	h264Format := &format.H264{
		PayloadTyp:        96,
		PacketizationMode: 1,
		SPS:               track.SPS,
		PPS:               track.PPS,
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
		sps:         track.SPS,
		pps:         track.PPS,
		decodeCh:    make(chan []byte, 1),
		done:        make(chan struct{}),
	}

	go sc.decodeLoop()

	slog.Info("snapshot consumer enabled for main stream", "camera", camera)
	return sc
}

// decodeLoop runs in a dedicated goroutine, decoding NAL streams without
// blocking the RTP fan-out callback. It owns sc.frameDec for its lifetime,
// confining all decoder use (and the final Close) to this single goroutine.
func (sc *SnapshotConsumer) decodeLoop() {
	defer sc.frameDec.Close()

	for {
		select {
		case nalStream, ok := <-sc.decodeCh:
			if !ok {
				return
			}
			ycbcr := sc.frameDec.Decode(nalStream)
			if ycbcr == nil {
				continue
			}
			rgba := ycbcrToRGBA(ycbcr)
			sc.mu.Lock()
			sc.lastFrame = rgba
			sc.lastTime = time.Now()
			sc.mu.Unlock()
		case <-sc.done:
			return
		}
	}
}

// LastFrame returns the most recently decoded full-resolution frame, or nil.
func (sc *SnapshotConsumer) LastFrame() *image.RGBA {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	return sc.lastFrame
}

// Close releases decoder resources. Safe to call more than once.
func (sc *SnapshotConsumer) Close() {
	sc.closeOnce.Do(func() {
		close(sc.done)
	})
}

// OnVideoRTP reassembles NAL units and queues IDR frames for async decode.
func (sc *SnapshotConsumer) OnVideoRTP(pkt *rtp.Packet) {
	if sc.h264Decoder == nil {
		return
	}

	au, _, err := sc.h264Decoder.Decode(pkt)
	if err != nil {
		return
	}

	// Rate limit: at most 1 decode per second
	sc.mu.RLock()
	tooSoon := time.Since(sc.lastTime) < time.Second
	sc.mu.RUnlock()
	if tooSoon {
		return
	}

	if !h264.IsRandomAccess(au) {
		return
	}

	// Update SPS/PPS from in-band parameters
	for _, nalu := range au {
		if len(nalu) == 0 {
			continue
		}
		typ := h264.NALUType(nalu[0] & 0x1F)
		switch typ {
		case h264.NALUTypeSPS:
			sc.sps = nalu
		case h264.NALUTypePPS:
			sc.pps = nalu
		}
	}

	if sc.sps == nil {
		return
	}

	// Build NAL stream with start codes, prepend SPS/PPS if needed
	var nalStream []byte
	startCode := []byte{0, 0, 0, 1}

	hasSPS := false
	for _, nalu := range au {
		if len(nalu) > 0 && h264.NALUType(nalu[0]&0x1F) == h264.NALUTypeSPS {
			hasSPS = true
			break
		}
	}
	if !hasSPS && sc.sps != nil {
		nalStream = append(nalStream, startCode...)
		nalStream = append(nalStream, sc.sps...)
		if sc.pps != nil {
			nalStream = append(nalStream, startCode...)
			nalStream = append(nalStream, sc.pps...)
		}
	}

	for _, nalu := range au {
		if len(nalu) == 0 {
			continue
		}
		nalStream = append(nalStream, startCode...)
		nalStream = append(nalStream, nalu...)
	}

	// Non-blocking send — drop frame if decode goroutine is busy
	select {
	case sc.decodeCh <- nalStream:
	default:
	}
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
