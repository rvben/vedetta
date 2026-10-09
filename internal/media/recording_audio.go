package media

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/pion/rtp"
	"github.com/rvben/vedetta/internal/aac"
	"github.com/rvben/vedetta/internal/audio"
	"github.com/rvben/vedetta/internal/rtsp"
)

const recordingAudioRate = 48000

var newRecordingAACEncoder = aac.New

// Each contiguous RTP run has its own encoder. A loss never joins unrelated
// waveform edges or compresses missing audio out of the recording timeline.
type timedAudio struct {
	packet   *rtp.Packet
	received time.Time
}

type recordingAudio struct {
	track          rtsp.TrackInfo
	encoder        aac.RecordingEncoder
	resampler      *audio.Upsampler
	factor         int
	skip           int
	input, written uint64
	base           uint64
	previous       *rtp.Packet
}

func newRecordingAudio(track *rtsp.TrackInfo) (*recordingAudio, error) {
	if track.ChannelCount > 1 || track.ClockRate <= 0 || recordingAudioRate%track.ClockRate != 0 || recordingAudioRate/track.ClockRate > 6 {
		return nil, fmt.Errorf("unsupported recording audio: %s/%d, channels=%d", track.Codec, track.ClockRate, track.ChannelCount)
	}
	r := &recordingAudio{track: *track, factor: recordingAudioRate / track.ClockRate}
	if err := r.restart(0); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *recordingAudio) restart(base uint64) error {
	enc, err := newRecordingAACEncoder(recordingAudioRate, 1)
	if err != nil {
		return err
	}
	r.encoder = enc
	r.resampler = audio.NewUpsampler(r.factor)
	r.skip = enc.Delay() / 1024
	r.input = 0
	r.written = 0
	r.base = base
	return nil
}

// AudioError exposes codec failures without turning them into video write
// failures. The next segment attempts a fresh encoder.
func (sw *SegmentWriter) AudioError() error {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.audioErr
}

func (sw *SegmentWriter) failAudio(err error) {
	if sw.audioErr != nil {
		return
	}
	sw.audioErr = err
	if sw.recordingAudio != nil && sw.recordingAudio.encoder != nil {
		sw.recordingAudio.encoder.Close()
		sw.recordingAudio.encoder = nil
	}
	sw.preAudio = nil
	slog.Error("recording audio unavailable; video recording continues", "path", sw.path, "error", err)
}

func (sw *SegmentWriter) appendEncodedAudio(frames [][]byte) {
	r := sw.recordingAudio
	for _, payload := range frames {
		if r.skip > 0 {
			r.skip--
			continue
		}
		remaining := r.input - r.written
		if remaining == 0 {
			continue
		}
		duration := uint64(1024)
		if remaining < duration {
			duration = remaining
		}
		if len(sw.pendingAudioSamples) == 0 {
			sw.pendingAudioDTS = r.base + r.written
		}
		sw.pendingAudioSamples = append(sw.pendingAudioSamples, &fmp4.Sample{Duration: uint32(duration), Payload: payload})
		sw.pendingBytes += len(payload)
		r.written += duration
		sw.audioDTS = r.base + r.written
	}
}

func (sw *SegmentWriter) finishRecordingAudio() {
	r := sw.recordingAudio
	if r == nil || r.encoder == nil {
		return
	}
	defer func() {
		if r.encoder != nil {
			r.encoder.Close()
			r.encoder = nil
		}
	}()
	frames, err := r.encoder.Encode(r.resampler.Flush())
	if err != nil {
		sw.failAudio(fmt.Errorf("flush audio resampler: %w", err))
		return
	}
	sw.appendEncodedAudio(frames)
	frames, err = r.encoder.Flush()
	if err != nil {
		sw.failAudio(fmt.Errorf("drain recording AAC: %w", err))
		return
	}
	sw.appendEncodedAudio(frames)
}

// writeG711Audio is called with sw.mu held. Only mux/filesystem errors escape.
func (sw *SegmentWriter) writeG711Audio(pkt *rtp.Packet) error {
	r := sw.recordingAudio
	if r == nil || r.encoder == nil || pkt == nil || len(pkt.Payload) == 0 || len(pkt.Payload) > 8192 || pkt.PayloadType != r.track.PayloadType {
		return nil
	}
	if !sw.initWritten {
		if sw.hasPendingVideoTime && len(sw.preAudio) < 16 {
			sw.preAudio = append(sw.preAudio, timedAudio{packet: pkt.Clone(), received: time.Now()})
		}
		return nil
	}
	if r.previous == nil {
		r.base = sw.videoDTS * recordingAudioRate / uint64(sw.videoTimeScale)
	} else {
		seq := int16(pkt.SequenceNumber - r.previous.SequenceNumber)
		if pkt.SSRC == r.previous.SSRC && seq <= 0 && seq >= -64 {
			return nil
		}
		delta := int32(pkt.Timestamp - r.previous.Timestamp)
		if pkt.SSRC != r.previous.SSRC || seq != 1 || delta != int32(len(r.previous.Payload)) {
			next := r.base + r.input
			if pkt.SSRC == r.previous.SSRC && delta >= int32(len(r.previous.Payload)) && delta <= int32(r.track.ClockRate*10) {
				next = r.base + r.input + uint64(delta-int32(len(r.previous.Payload)))*uint64(r.factor)
			} else {
				video := sw.videoDTS * recordingAudioRate / uint64(sw.videoTimeScale)
				if video > next {
					next = video
				}
			}
			sw.finishRecordingAudio()
			if err := sw.flushGOP(); err != nil {
				return err
			}
			if sw.audioErr != nil {
				return nil
			}
			if err := r.restart(next); err != nil {
				sw.failAudio(err)
				return nil
			}
		}
	}
	// Keep only header and packet length for continuity checks; don't retain
	// caller-owned RTP payloads after returning.
	r.previous = &rtp.Packet{Header: pkt.Header, Payload: make([]byte, len(pkt.Payload))}
	r.input += uint64(len(pkt.Payload) * r.factor)
	frames, err := r.encoder.Encode(r.resampler.Process(audio.DecodeG711(pkt.Payload, r.track.Codec == "PCMA")))
	if err != nil {
		sw.failAudio(fmt.Errorf("encode recording AAC: %w", err))
		return nil
	}
	sw.appendEncodedAudio(frames)
	return sw.capPendingPart()
}
