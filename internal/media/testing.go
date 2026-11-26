package media

import (
	"image"
	"time"
)

// NewTestSnapshotConsumer returns a SnapshotConsumer whose LastFrame reports the
// given frame as having just arrived, with no RTP decoder and no frame decoder.
// It lets tests stand in a camera's full-resolution snapshot source without
// RTSP or OpenH264 wiring.
func NewTestSnapshotConsumer(frame *image.RGBA) *SnapshotConsumer {
	return &SnapshotConsumer{lastFrame: frame, lastTime: time.Now()}
}

// NewTestSnapshotConsumerWithKeyframe returns a SnapshotConsumer holding one
// keyframe that has just arrived and is not decoded yet. The first LastFrame
// hands it to dec, so a test can observe when, and on which goroutine, the
// decode happens.
func NewTestSnapshotConsumerWithKeyframe(dec FrameDecoder) *SnapshotConsumer {
	return &SnapshotConsumer{frameDec: dec, unit: keyUnit{data: []byte{0, 0, 0, 1, 0x65}, at: time.Now(), seq: 1}, seq: 1}
}
