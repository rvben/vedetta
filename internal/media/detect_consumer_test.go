package media

import (
	"image"
	"strings"
	"testing"
	"time"

	"github.com/rvben/vedetta/internal/metrics"
)

type countingFrameDecoder struct {
	decodes int
	frame   *image.YCbCr
}

func (d *countingFrameDecoder) Decode([]byte) *image.YCbCr {
	d.decodes++
	return d.frame
}

func (*countingFrameDecoder) Flush() *image.YCbCr { return nil }
func (*countingFrameDecoder) Close()              {}

// dispatchFrame must deliver a decoded frame when the detection channel has
// room and drop-and-count it when the channel is full, so decoding never blocks
// on a busy detector. The drop counter is labelled by camera.
func TestDetectConsumerDispatchFrameCountsDrops(t *testing.T) {
	metrics.ResetForTest()
	t.Cleanup(metrics.ResetForTest)

	dc := &DetectConsumer{camera: "garage", frameCh: make(chan RawFrame, 1)}

	// First frame fits the buffered channel: delivered, not dropped.
	dc.dispatchFrame(RawFrame{Width: 1, Height: 1})
	// Channel now full; the next two frames must be dropped and counted.
	dc.dispatchFrame(RawFrame{Width: 1, Height: 1})
	dc.dispatchFrame(RawFrame{Width: 1, Height: 1})

	var b strings.Builder
	metrics.WriteProm(&b)
	out := b.String()

	if !strings.Contains(out, `vedetta_detect_input_dropped_total{camera="garage"} 2`) {
		t.Errorf("expected 2 dropped frames for garage:\n%s", out)
	}
	if len(dc.frameCh) != 1 {
		t.Errorf("expected 1 frame delivered to channel, got %d", len(dc.frameCh))
	}
}

func TestDetectConsumerDecodesEveryAccessUnitBeforeDispatchThrottling(t *testing.T) {
	frame := image.NewYCbCr(image.Rect(0, 0, 2, 2), image.YCbCrSubsampleRatio420)
	decoder := &countingFrameDecoder{frame: frame}
	dc := &DetectConsumer{
		width:        2,
		height:       2,
		camera:       "front_door",
		h264Dec:      decoder,
		sps:          []byte{0x67, 0x42},
		frameCh:      make(chan RawFrame, 2),
		frameDelay:   time.Second,
		lastLog:      time.Now(),
		fpsWindowDur: 5 * time.Second,
	}

	start := time.Now()
	dc.processAccessUnit([][]byte{{0x65, 0x01}}, start)
	dc.processAccessUnit([][]byte{{0x41, 0x02}}, start.Add(10*time.Millisecond))

	if decoder.decodes != 2 {
		t.Fatalf("decoder calls = %d, want every access unit decoded", decoder.decodes)
	}
	if got := len(dc.frameCh); got != 1 {
		t.Fatalf("dispatched frames = %d, want only the first frame inside throttle window", got)
	}
}
