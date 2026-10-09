// Package aac provides the shared native AAC-LC encoder for live playback
// and recording. Calls on an encoder must be serialized by its owner.
package aac

type Encoder interface {
	Encode(pcm []int16) ([][]byte, error)
	Close()
}

// RecordingEncoder additionally exposes priming and end-of-stream draining.
type RecordingEncoder interface {
	Encoder
	Delay() int
	Flush() ([][]byte, error)
}

func New(sampleRate, channels int) (RecordingEncoder, error) {
	return newFDKAACEncoder(sampleRate, channels)
}
