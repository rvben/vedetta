package media

import (
	"log/slog"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/pion/rtp"

	"github.com/rvben/vedetta/internal/metrics"
	"github.com/rvben/vedetta/internal/rtsp"
)

// RawFrame holds a decoded RGB24 frame for detection.
type RawFrame struct {
	Data   []byte
	Width  int
	Height int
}

// DetectConsumer implements rtsp.Consumer and decodes H264 frames to RGB24.
type DetectConsumer struct {
	width  int
	height int
	camera string

	// decMu guards the lifecycle and use of the decoders against Close.
	// OnVideoRTP runs on the RTSP fan-out goroutine while Close runs on the
	// camera's readFrames goroutine. Without this, Close frees the underlying C
	// decoder (h264Dec) while OnVideoRTP is still using it - a use-after-free
	// inside the C library that corrupts the Go heap. Holds whether h264Dec is a
	// software (OpenH264) or hardware (VideoToolbox) backend.
	decMu       sync.Mutex
	h264Decoder *rtph264.Decoder
	h264Dec     FrameDecoder
	sps         []byte
	pps         []byte

	mu           sync.Mutex
	frameCh      chan RawFrame
	lastDispatch time.Time
	frameDelay   time.Duration
	frameCount   uint64
	lastLog      time.Time
	rtpCount     uint64
	auCount      uint64
	idrCount     uint64
	haveSync     bool
	available    bool

	// fpsWindow holds the timestamps of the most recent access units for
	// rolling FPS computation. Trimmed to the last fpsWindowDur on read.
	fpsWindow    []time.Time
	fpsWindowDur time.Duration
}

// NewDetectConsumer creates a consumer that decodes H264 for detection.
// Detection is disabled if the track cannot be decoded locally.
func NewDetectConsumer(camera string, width, height, fps int, track *rtsp.TrackInfo) *DetectConsumer {
	dc := &DetectConsumer{
		width:        width,
		height:       height,
		camera:       camera,
		frameCh:      make(chan RawFrame, 2),
		frameDelay:   time.Second / time.Duration(max(fps, 1)),
		lastLog:      time.Now(),
		fpsWindowDur: 5 * time.Second,
	}

	if track == nil || track.Codec != "H264" {
		slog.Warn("detection disabled: track is not H264")
		return dc
	}

	dc.sps = track.SPS
	dc.pps = track.PPS

	h264Format := &format.H264{
		PayloadTyp:        96,
		PacketizationMode: 1,
		SPS:               track.SPS,
		PPS:               track.PPS,
	}
	dec, err := h264Format.CreateDecoder()
	if err != nil {
		slog.Warn("failed to create H264 RTP decoder for detection", "error", err)
		return dc
	}
	dc.h264Decoder = dec

	dc.h264Dec = NewDefaultFrameDecoder(dc.sps, dc.pps)
	if dc.h264Dec == nil {
		slog.Warn("detection disabled: no H264 decoder available", "camera", camera)
		return dc
	}

	dc.available = true
	slog.Info("detection enabled", "camera", camera)
	return dc
}

// Available reports whether detection decode is operational for this stream.
func (dc *DetectConsumer) Available() bool {
	return dc.available
}

// SourceFPS returns the rolling-window frame rate (decoded access units per
// second) over the last fpsWindowDur. Returns 0 when the window is empty.
func (dc *DetectConsumer) SourceFPS() float64 {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	n := len(dc.fpsWindow)
	if n < 2 {
		return 0
	}
	span := dc.fpsWindow[n-1].Sub(dc.fpsWindow[0]).Seconds()
	if span <= 0 {
		return 0
	}
	return float64(n-1) / span
}

// Frames returns the channel of decoded frames.
func (dc *DetectConsumer) Frames() <-chan RawFrame {
	return dc.frameCh
}

// Close releases decoder resources. It blocks until any in-flight OnVideoRTP
// decode finishes, so the OpenH264 C decoder is never freed while in use.
func (dc *DetectConsumer) Close() {
	dc.decMu.Lock()
	defer dc.decMu.Unlock()
	if dc.h264Dec != nil {
		dc.h264Dec.Close()
		dc.h264Dec = nil
	}
}

// OnVideoRTP processes a video RTP packet, decoding frames to RGB24 after the
// first random-access unit has initialized decoder state.
func (dc *DetectConsumer) OnVideoRTP(pkt *rtp.Packet) {
	// Hold decMu for the whole call so Close cannot free the OpenH264 C decoder
	// mid-decode. OnVideoRTP is single-goroutine in production (the RTSP fan-out
	// goroutine), so this is uncontended except against Close at teardown.
	dc.decMu.Lock()
	defer dc.decMu.Unlock()

	if dc.h264Decoder == nil || dc.h264Dec == nil {
		return
	}

	dc.mu.Lock()
	dc.rtpCount++
	dc.mu.Unlock()

	au, err := dc.h264Decoder.Decode(pkt)
	if err != nil {
		return
	}

	now := time.Now()
	dc.mu.Lock()
	dc.auCount++
	cutoff := now.Add(-dc.fpsWindowDur)
	dc.fpsWindow = append(dc.fpsWindow, now)
	for len(dc.fpsWindow) > 0 && dc.fpsWindow[0].Before(cutoff) {
		dc.fpsWindow = dc.fpsWindow[1:]
	}
	dc.mu.Unlock()

	// Periodic status log — fires whether or not frames are being decoded
	dc.mu.Lock()
	if time.Since(dc.lastLog) >= 5*time.Minute {
		slog.Info("detection status",
			"camera", dc.camera,
			"rtp_packets", dc.rtpCount,
			"access_units", dc.auCount,
			"idr_frames", dc.idrCount,
			"frames_decoded", dc.frameCount,
		)
		dc.frameCount = 0
		dc.rtpCount = 0
		dc.auCount = 0
		dc.idrCount = 0
		dc.lastLog = time.Now()
	}
	dc.mu.Unlock()

	dc.processAccessUnit(au, now)
}

// processAccessUnit feeds every decodable access unit into the stateful H.264
// decoder, then rate-limits only the expensive RGB conversion and downstream
// detection dispatch. Predictive H.264 frames reference earlier access units;
// dropping compressed frames before Decode corrupts the reference picture and
// makes the decoder conceal moving regions with stale macroblocks.
func (dc *DetectConsumer) processAccessUnit(au [][]byte, now time.Time) {
	isRandomAccess := h264.IsRandomAccess(au)

	dc.mu.Lock()
	haveSync := dc.haveSync
	dc.mu.Unlock()

	if !haveSync && !isRandomAccess {
		return
	}
	if isRandomAccess {
		dc.mu.Lock()
		dc.idrCount++
		dc.haveSync = true
		dc.mu.Unlock()
	}

	// Update SPS/PPS from in-band parameters
	for _, nalu := range au {
		if len(nalu) == 0 {
			continue
		}
		typ := h264.NALUType(nalu[0] & 0x1F)
		switch typ {
		case h264.NALUTypeSPS:
			dc.sps = nalu
		case h264.NALUTypePPS:
			dc.pps = nalu
		}
	}

	if dc.sps == nil {
		return
	}

	// Build NAL unit stream with start codes for OpenH264.
	// Always prepend SPS/PPS before IDR if not already present in the AU,
	// as some cameras (Tapo) send IDR without inline parameter sets.
	var nalStream []byte
	startCode := []byte{0, 0, 0, 1}

	hasSPS := false
	for _, nalu := range au {
		if len(nalu) > 0 && h264.NALUType(nalu[0]&0x1F) == h264.NALUTypeSPS {
			hasSPS = true
			break
		}
	}
	if !hasSPS && dc.sps != nil {
		nalStream = append(nalStream, startCode...)
		nalStream = append(nalStream, dc.sps...)
		if dc.pps != nil {
			nalStream = append(nalStream, startCode...)
			nalStream = append(nalStream, dc.pps...)
		}
	}

	for _, nalu := range au {
		if len(nalu) == 0 {
			continue
		}
		nalStream = append(nalStream, startCode...)
		nalStream = append(nalStream, nalu...)
	}

	decodeStart := time.Now()
	ycbcr := dc.h264Dec.Decode(nalStream)
	metrics.FrameDecodeDuration.Observe(dc.camera, time.Since(decodeStart))
	if ycbcr == nil {
		return
	}
	metrics.FramesDecoded.Inc(dc.camera)

	dc.mu.Lock()
	dc.frameCount++
	if !dc.lastDispatch.IsZero() && now.Sub(dc.lastDispatch) < dc.frameDelay {
		dc.mu.Unlock()
		return
	}
	dc.lastDispatch = now
	dc.mu.Unlock()

	rgb24 := ycbcrToRGB24Scaled(ycbcr, dc.width, dc.height)
	dc.dispatchFrame(RawFrame{Data: rgb24, Width: dc.width, Height: dc.height})
}

// dispatchFrame hands a decoded frame to the detection goroutine. It drops the
// frame (and counts the drop) when the channel is full, so decoding never
// blocks on a busy detector. A rising drop count is the detection-backpressure
// signal.
func (dc *DetectConsumer) dispatchFrame(frame RawFrame) {
	select {
	case dc.frameCh <- frame:
	default:
		metrics.DetectInputDropped.Inc(dc.camera)
	}
}

// OnAudioRTP is a no-op for detection.
func (dc *DetectConsumer) OnAudioRTP(_ *rtp.Packet) {}

// OnDisconnect is called when the source disconnects.
func (dc *DetectConsumer) OnDisconnect() {}
