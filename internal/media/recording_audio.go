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

// recordingAudioMaxLeadMs is how far the RTP timestamps of in-order packets may
// run ahead of the samples received before the difference counts as missing
// audio. Cameras that stamp audio from a wall clock jitter around the sample
// count by more than 100 ms without losing a sample, and restarting the encoder
// for that splits the recording into one fragment per packet.
const recordingAudioMaxLeadMs = 500

var newRecordingAACEncoder = aac.New

// Each contiguous RTP run has its own encoder. A loss never joins unrelated
// waveform edges or compresses missing audio out of the recording timeline.
// Packets that arrive in order belong to one run until their timestamps show
// that audio is missing.
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
	// lead is how far, in RTP clock units, the timestamps of the current run
	// have advanced beyond the samples it received.
	lead     int64
	previous *rtp.Packet
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
	r.lead = 0
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
		step := int64(int32(pkt.Timestamp - r.previous.Timestamp))
		sameSource := pkt.SSRC == r.previous.SSRC
		inOrder := sameSource && seq == 1
		maxLead := int64(r.track.ClockRate) * recordingAudioMaxLeadMs / 1000
		clockReset := step > int64(r.track.ClockRate*10)
		// gap is the audio missing before this packet. After a loss it is what
		// the timestamp step leaves beyond the previous packet's samples.
		gap := step - int64(len(r.previous.Payload))
		if inOrder {
			// Nothing was lost, so the samples are continuous whatever each
			// timestamp says: only the lead accumulated over the run can show
			// missing audio. A clock that steps back starts a new measurement.
			r.lead += gap
			if step < -maxLead {
				r.lead = 0
			}
			gap = r.lead
		}
		if !inOrder || gap > maxLead || clockReset {
			next := r.base + r.input
			if sameSource && gap >= 0 && !clockReset {
				next += uint64(gap) * uint64(r.factor)
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
