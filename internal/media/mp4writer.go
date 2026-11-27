package media

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpmpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/pion/rtp"

	"github.com/rvben/vedetta/internal/h264au"
	"github.com/rvben/vedetta/internal/rtsp"
)

// ErrTimestampGap reports a video RTP timestamp discontinuity (forward jump of
// 2s or more, or a backwards jump) within one segment file. Media time inside
// a segment must advance 1:1 with wall time so playback can map a wall-clock
// instant to a media offset by subtraction; substituting a fake sample
// duration would silently compress the gap and desync that mapping for the
// rest of the file. The last frame before the gap is in the segment when this
// is returned. Callers should close the segment and start a new one, which
// takes over the frame after the gap through TakeCarry and WriteCarry.
var ErrTimestampGap = errors.New("video RTP timestamp gap")

// ErrSegmentSplit reports that WriteVideo ended the segment on a frame
// boundary because the caller asked for it (SplitAtNextKeyframe,
// SplitAtNextFrame). What belongs to the next segment waits in TakeCarry for
// that segment's WriteCarry; every packet consumed so far of the frame being
// assembled travels with it.
var ErrSegmentSplit = errors.New("segment split on a frame boundary")

// VideoCarry is what one segment hands to the next when it is split: the
// keyframe it withheld, so the second file starts where the first one stops,
// and the depacketizer. A frame is only known to be complete once the frame
// after it has delivered a whole NAL unit, which can be several packets in,
// so the depacketizer goes along with everything it holds of that next frame.
// A split forced on a frame that is no keyframe carries no frame, and neither
// does a timestamp gap.
type VideoCarry struct {
	au       [][]byte
	duration uint32
	received time.Time
	sps, pps []byte

	decoder          *rtsp.H264AccessUnitDecoder
	pendingTimestamp uint32
	pendingTime      time.Time
}

// Received returns when the first packet of the carried keyframe arrived, or
// of the frame still being assembled when no keyframe is carried.
func (c *VideoCarry) Received() time.Time { return c.received }

const (
	// maxPendingPartSeconds bounds how much media time may sit in the GOP
	// buffer before it is written out. A stream that stops emitting keyframes
	// (or emits them far apart) would otherwise buffer an entire segment in
	// memory, since a part is normally only flushed on the next keyframe.
	maxPendingPartSeconds = 4
	// maxPendingPartBytes bounds the same buffer by size, which is what
	// catches a high-bitrate stream well before the duration ceiling.
	maxPendingPartBytes = 8 << 20
	// noKeyframeLogInterval rate-limits the warning about a stream whose
	// keyframe interval exceeds maxPendingPartSeconds.
	noKeyframeLogInterval = time.Minute
)

// SegmentWriter writes RTP packets into an fMP4 file.
// Video and audio samples are buffered per GOP (group of pictures) and flushed
// as a single fMP4 Part when a new keyframe arrives or the segment is closed.
// This produces one moof+mdat per GOP instead of per frame, which is essential
// for smooth HLS byte-range playback.
type SegmentWriter struct {
	mu   sync.Mutex
	path string
	f    *os.File

	videoTrackID int
	audioTrackID int

	h264Format  *format.H264
	h264Decoder *rtsp.H264AccessUnitDecoder
	videoSPS    []byte
	videoPPS    []byte

	aacFormat      *format.MPEG4Audio
	aacDecoder     *rtpmpeg4audio.Decoder
	aacConfig      *mpeg4audio.AudioSpecificConfig
	recordingAudio *recordingAudio
	audioErr       error
	preAudio       []timedAudio

	initWritten       bool
	seqNum            uint32
	videoDTS          uint64
	lastVideoDuration uint32
	audioDTS          uint64
	startTime         time.Time
	firstSampleTime   time.Time // wall time of the keyframe that opened the file
	hasAudio          bool
	videoTimeScale    uint32
	audioTimeScale    uint32

	// Arrival of the first packet of the newest frame, and of the frame before
	// it: a frame leaves the depacketizer only after the next one has started.
	pendingVideoTimestamp uint32
	pendingVideoTime      time.Time
	hasPendingVideoTime   bool
	prevVideoTimestamp    uint32
	prevVideoTime         time.Time
	hasPrevVideoTime      bool
	skipDecoderFlush      bool
	splitRequested        bool
	splitAnyFrame         bool
	carry                 *VideoCarry

	// GOP buffering: accumulate samples until next keyframe
	pendingVideoSamples []*fmp4.Sample
	pendingAudioSamples []*fmp4.Sample
	pendingVideoDTS     uint64 // base decode time for pending video GOP
	pendingAudioDTS     uint64 // base decode time for pending audio
	pendingBytes        int    // payload bytes buffered but not yet marshaled
	flushedVideoDTS     uint64 // video media time actually marshaled to the file
	lastNoKeyframeLog   time.Time
}

// NewSegmentWriter creates a new fMP4 segment writer.
func NewSegmentWriter(path string, video, audio *rtsp.TrackInfo) (*SegmentWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("create segment file: %w", err)
	}

	sw := &SegmentWriter{
		path:           path,
		f:              f,
		videoTrackID:   1,
		audioTrackID:   2,
		startTime:      time.Now(),
		videoTimeScale: 90000,
		audioTimeScale: 90000,
	}

	if video != nil && video.Codec == "H264" {
		sw.videoSPS = video.SPS
		sw.videoPPS = video.PPS

		sw.h264Format = &format.H264{
			PayloadTyp:        96,
			PacketizationMode: 1,
			SPS:               video.SPS,
			PPS:               video.PPS,
		}
		dec, err := sw.h264Format.CreateDecoder()
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("create H264 decoder: %w", err)
		}
		sw.h264Decoder = rtsp.NewH264AccessUnitDecoder(dec)
	}

	if audio != nil && audio.Codec == "AAC" {
		sw.hasAudio = true
		sw.audioTimeScale = uint32(audio.ClockRate)

		channels := audio.ChannelCount
		if channels <= 0 {
			channels = 1
		}
		channelConfig := uint8(channels)
		if channels == 8 {
			channelConfig = 7
		}

		sw.aacFormat = &format.MPEG4Audio{
			PayloadTyp: 97,
			Config: &mpeg4audio.AudioSpecificConfig{
				Type:          mpeg4audio.ObjectTypeAACLC,
				SampleRate:    audio.ClockRate,
				ChannelConfig: channelConfig,
			},
			SizeLength:       13,
			IndexLength:      3,
			IndexDeltaLength: 3,
		}
		dec, err := sw.aacFormat.CreateDecoder()
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("create AAC decoder: %w", err)
		}
		sw.aacDecoder = dec

		sw.aacConfig = &mpeg4audio.AudioSpecificConfig{
			Type:          mpeg4audio.ObjectTypeAACLC,
			SampleRate:    audio.ClockRate,
			ChannelConfig: channelConfig,
		}
	}

	if audio != nil && (audio.Codec == "PCMA" || audio.Codec == "PCMU") {
		r, err := newRecordingAudio(audio)
		if err != nil {
			sw.failAudio(err)
		} else {
			sw.recordingAudio = r
			sw.hasAudio = true
			sw.audioTimeScale = recordingAudioRate
			sw.aacConfig = &mpeg4audio.AudioSpecificConfig{Type: mpeg4audio.ObjectTypeAACLC, SampleRate: recordingAudioRate, ChannelConfig: 1}
		}
	}
	if audio != nil && audio.Codec != "AAC" && audio.Codec != "PCMA" && audio.Codec != "PCMU" {
		sw.failAudio(fmt.Errorf("unsupported recording audio codec: %s", audio.Codec))
	}
	return sw, nil
}

// WriteVideo processes a video RTP packet into the fMP4 segment.
func (sw *SegmentWriter) WriteVideo(pkt *rtp.Packet) error {
	if sw.h264Decoder == nil {
		return nil
	}
	// Preserve the recording consumer's per-packet panic containment contract
	// without taking sw.mu first; a panic while holding the mutex would poison
	// the writer and deadlock its recovery-time Close.
	if pkt == nil {
		panic("nil H264 RTP packet")
	}

	now := time.Now()
	sw.mu.Lock()
	if !sw.hasPendingVideoTime {
		sw.pendingVideoTimestamp = pkt.Timestamp
		sw.pendingVideoTime = now
		sw.hasPendingVideoTime = true
	} else if pkt.Timestamp != sw.pendingVideoTimestamp {
		sw.prevVideoTimestamp, sw.prevVideoTime, sw.hasPrevVideoTime = sw.pendingVideoTimestamp, sw.pendingVideoTime, true
		sw.pendingVideoTimestamp = pkt.Timestamp
		sw.pendingVideoTime = now
	}
	sw.mu.Unlock()

	au, rtpTimestamp, err := sw.h264Decoder.Decode(pkt)
	if err != nil {
		return nil
	}
	if len(au) == 0 {
		return nil
	}

	sampleDuration := pkt.Timestamp - rtpTimestamp
	if sampleDuration == 0 {
		sampleDuration = sw.videoTimeScale / 30
	} else if sampleDuration >= sw.videoTimeScale*2 {
		// The frame that just left the depacketizer is the last one before the
		// discontinuity. It is a real frame and ends this segment, shown for
		// as long as the frame before it, exactly as Close ends a segment on
		// the frame it flushes; the gap itself is never given a duration. The
		// decoder holds every packet received so far of the first frame after
		// the discontinuity, so it moves to the next segment and Close must
		// not flush it into this file.
		sw.mu.Lock()
		defer sw.mu.Unlock()
		sw.carry = &VideoCarry{
			received: sw.pendingVideoTime,
			decoder:  sw.h264Decoder, pendingTimestamp: sw.pendingVideoTimestamp, pendingTime: sw.pendingVideoTime,
		}
		sw.skipDecoderFlush = true
		lastFrameTime := sw.pendingVideoTime
		if sw.hasPrevVideoTime {
			lastFrameTime = sw.prevVideoTime
		}
		if err := sw.writeVideoAccessUnit(au, sw.lastVideoDuration, lastFrameTime); err != nil {
			return errors.Join(ErrTimestampGap, err)
		}
		return ErrTimestampGap
	}

	sw.mu.Lock()
	defer sw.mu.Unlock()
	// The frame that just left the depacketizer is usually the one before the
	// newest: its last packet is followed by the packets that complete the
	// first NAL unit of the next frame.
	accessUnitTime := sw.pendingVideoTime
	if sw.hasPrevVideoTime && rtpTimestamp != sw.pendingVideoTimestamp {
		accessUnitTime = sw.prevVideoTime
	}
	keyframe := h264.IsRandomAccess(h264au.DropSEI(au))
	if sw.splitRequested && sw.initWritten && (keyframe || sw.splitAnyFrame) {
		// The decoder already holds the start of the next frame, so it moves
		// to the next segment and Close must not flush it into this file.
		carry := &VideoCarry{
			received: sw.pendingVideoTime,
			decoder:  sw.h264Decoder, pendingTimestamp: sw.pendingVideoTimestamp, pendingTime: sw.pendingVideoTime,
		}
		if keyframe {
			carry.au, carry.duration, carry.received = au, sampleDuration, accessUnitTime
			carry.sps, carry.pps, _ = h264au.TrackParameterSets(au, sw.videoSPS, sw.videoPPS)
		} else if err := sw.writeVideoAccessUnit(au, sampleDuration, accessUnitTime); err != nil {
			return err
		}
		sw.carry = carry
		sw.skipDecoderFlush = true
		return ErrSegmentSplit
	}
	return sw.writeVideoAccessUnit(au, sampleDuration, accessUnitTime)
}

// SplitAtNextKeyframe asks WriteVideo to withhold the next keyframe and return
// ErrSegmentSplit instead of writing it. It reports false when this writer can
// never do so: it has no H.264 track, or no keyframe has opened the file yet.
func (sw *SegmentWriter) SplitAtNextKeyframe() bool {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if sw.h264Decoder == nil || !sw.initWritten {
		return false
	}
	sw.splitRequested = true
	return true
}

// SplitAtNextFrame asks WriteVideo to end the segment at the next frame
// boundary whatever the frame is: a keyframe is withheld for the next segment,
// any other frame is written here first. It reports false as
// SplitAtNextKeyframe does.
func (sw *SegmentWriter) SplitAtNextFrame() bool {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if sw.h264Decoder == nil || !sw.initWritten {
		return false
	}
	sw.splitRequested, sw.splitAnyFrame = true, true
	return true
}

// TakeCarry returns what the WriteVideo call that returned ErrSegmentSplit or
// ErrTimestampGap left for the next segment, once.
func (sw *SegmentWriter) TakeCarry() *VideoCarry {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	c := sw.carry
	sw.carry = nil
	return c
}

// WriteCarry continues the previous segment's stream in this one: it writes
// the keyframe withheld there, if any, as this segment's first sample, with
// the arrival time and duration it had, and goes on with the frame being
// assembled in the depacketizer that came along.
func (sw *SegmentWriter) WriteCarry(c *VideoCarry) error {
	if c == nil {
		return nil
	}
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if c.sps != nil && c.pps != nil {
		sw.videoSPS, sw.videoPPS = c.sps, c.pps
	}
	sw.h264Decoder = c.decoder
	sw.pendingVideoTimestamp, sw.pendingVideoTime, sw.hasPendingVideoTime = c.pendingTimestamp, c.pendingTime, true
	if len(c.au) == 0 {
		return nil
	}
	return sw.writeVideoAccessUnit(c.au, c.duration, c.received)
}

// writeVideoAccessUnit writes one timestamp-coalesced H.264 frame. sw.mu must
// be held by the caller.
func (sw *SegmentWriter) writeVideoAccessUnit(au [][]byte, sampleDuration uint32, sampleTime time.Time) error {

	// Strip SEI before muxing. A recorded clip is replayed by the same strict
	// decoders that reject junk SEI live (an iPhone opening an event clip runs
	// the identical VideoToolbox path), so the file on disk carries the same
	// normalized bitstream the live transports serve. See h264au.DropSEI.
	au = h264au.DropSEI(au)
	if len(au) == 0 {
		return nil
	}

	// Update SPS/PPS from in-band parameters
	sw.videoSPS, sw.videoPPS, _ = h264au.TrackParameterSets(au, sw.videoSPS, sw.videoPPS)

	// Write the init segment on first keyframe
	if !sw.initWritten {
		if !h264.IsRandomAccess(au) {
			sw.preAudio = nil
			return nil
		}
		if sw.videoSPS == nil || sw.videoPPS == nil {
			return nil
		}
		if err := sw.writeInit(); err != nil {
			return err
		}
		sw.initWritten = true
		sw.firstSampleTime = sampleTime
		queued := sw.preAudio
		sw.preAudio = nil
		for _, p := range queued {
			if !p.received.Before(sampleTime) {
				if err := sw.writeG711Audio(p.packet); err != nil {
					return err
				}
			}
		}
	}

	if sampleDuration == 0 {
		sampleDuration = sw.videoTimeScale / 30
	}

	avcc, err := h264.AVCC(au).Marshal()
	if err != nil {
		return fmt.Errorf("fill H264 sample: %w", err)
	}
	sample := &fmp4.Sample{
		Duration:        sampleDuration,
		IsNonSyncSample: !h264.IsRandomAccess(au),
		Payload:         avcc,
	}

	// On keyframe: flush the previous GOP before starting a new one
	if h264.IsRandomAccess(au) && len(sw.pendingVideoSamples) > 0 {
		if err := sw.flushGOP(); err != nil {
			return err
		}
	}

	// Start tracking base DTS for this GOP if this is the first sample
	if len(sw.pendingVideoSamples) == 0 {
		sw.pendingVideoDTS = sw.videoDTS
	}

	sw.pendingVideoSamples = append(sw.pendingVideoSamples, sample)
	sw.pendingBytes += len(sample.Payload)
	sw.videoDTS += uint64(sample.Duration)
	sw.lastVideoDuration = sample.Duration

	return sw.capPendingPart()
}

// capPendingPart writes the buffered samples out early when the GOP buffer
// grows past its duration or byte ceiling. fMP4 parts that begin on a
// non-keyframe are legal: mp4reader derives seek points from per-sample sync
// flags (tfhd default_sample_flags / trun sample flags), not from fragment
// boundaries, so an extra part only extends the current HLS segment instead of
// starting one. Called with sw.mu held.
func (sw *SegmentWriter) capPendingPart() error {
	if len(sw.pendingVideoSamples) == 0 && len(sw.pendingAudioSamples) == 0 {
		return nil
	}

	overBytes := sw.pendingBytes >= maxPendingPartBytes
	overTime := false
	if len(sw.pendingVideoSamples) > 0 && sw.videoTimeScale > 0 {
		buffered := sw.videoDTS - sw.pendingVideoDTS
		overTime = buffered >= uint64(sw.videoTimeScale)*maxPendingPartSeconds
	}
	if !overBytes && !overTime {
		return nil
	}

	if overTime && time.Since(sw.lastNoKeyframeLog) > noKeyframeLogInterval {
		buffered := time.Duration(sw.videoDTS-sw.pendingVideoDTS) * time.Second / time.Duration(sw.videoTimeScale)
		slog.Warn("no keyframe within the expected interval, flushing a partial GOP",
			"path", sw.path,
			"buffered", buffered.Round(time.Millisecond),
			"samples", len(sw.pendingVideoSamples),
		)
		sw.lastNoKeyframeLog = time.Now()
	}

	return sw.flushGOP()
}

// WriteAudio processes an audio RTP packet into the fMP4 segment.
func (sw *SegmentWriter) WriteAudio(pkt *rtp.Packet) error {
	if sw.recordingAudio != nil {
		sw.mu.Lock()
		defer sw.mu.Unlock()
		return sw.writeG711Audio(pkt)
	}
	if sw.aacDecoder == nil {
		return nil
	}

	aus, err := sw.aacDecoder.Decode(pkt)
	if err != nil {
		return nil
	}

	sw.mu.Lock()
	defer sw.mu.Unlock()

	if !sw.initWritten {
		return nil
	}

	for _, au := range aus {
		sample := &fmp4.Sample{
			Duration: 1024, // Standard AAC frame size in samples
			Payload:  au,
		}

		if len(sw.pendingAudioSamples) == 0 {
			sw.pendingAudioDTS = sw.audioDTS
		}

		sw.pendingAudioSamples = append(sw.pendingAudioSamples, sample)
		sw.pendingBytes += len(sample.Payload)
		sw.audioDTS += 1024
	}

	return sw.capPendingPart()
}

// StartTime returns the wall-clock time recording actually began: the arrival
// of the keyframe that opened the file. Falls back to the writer's creation
// time when no sample has been written yet.
func (sw *SegmentWriter) StartTime() time.Time {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if !sw.firstSampleTime.IsZero() {
		return sw.firstSampleTime
	}
	if sw.hasPendingVideoTime {
		return sw.pendingVideoTime
	}
	return sw.startTime
}

// HasMedia reports whether a video sample has been accepted into the segment.
// A writer that is still waiting for its opening keyframe holds no media, so
// closing it would produce an empty file rather than preserve anything.
func (sw *SegmentWriter) HasMedia() bool {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return !sw.firstSampleTime.IsZero()
}

// FlushedDuration returns the video media time that has actually reached the
// file. It lags the writer's total whenever a flush fails, which lets a caller
// describe a truncated segment without claiming coverage that was never
// written. Returns 0 when no video sample has been marshaled.
func (sw *SegmentWriter) FlushedDuration() time.Duration {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if sw.videoTimeScale == 0 || sw.flushedVideoDTS == 0 {
		return 0
	}
	return time.Duration(sw.flushedVideoDTS * uint64(time.Second) / uint64(sw.videoTimeScale))
}

// Close finalizes the segment and returns its duration: the media time written
// to the video track when samples exist (wall-clock age would overstate it
// whenever the stream stalled), else wall time since creation.
func (sw *SegmentWriter) Close() (time.Duration, error) {
	var flushErr error
	sw.mu.Lock()
	lastVideoDuration := sw.lastVideoDuration
	sw.mu.Unlock()
	if lastVideoDuration == 0 {
		lastVideoDuration = sw.videoTimeScale / 30
	}
	if sw.h264Decoder != nil && !sw.skipDecoderFlush {
		if au, _, err := sw.h264Decoder.Flush(); err != nil {
			flushErr = err
		} else if len(au) > 0 {
			sw.mu.Lock()
			flushErr = sw.writeVideoAccessUnit(au, lastVideoDuration, sw.pendingVideoTime)
			sw.mu.Unlock()
		}
	}

	sw.mu.Lock()
	defer sw.mu.Unlock()

	sw.finishRecordingAudio()
	sw.preAudio = nil

	// Flush any remaining buffered samples. This is the only write for the
	// last GOP of the segment, so its failure must reach the caller: silently
	// dropping it registers a truncated file as a complete segment.
	var gopErr error
	if len(sw.pendingVideoSamples) > 0 || len(sw.pendingAudioSamples) > 0 {
		gopErr = sw.flushGOP()
	}

	duration := time.Since(sw.startTime)
	if sw.videoDTS > 0 && sw.videoTimeScale > 0 {
		duration = time.Duration(sw.videoDTS * uint64(time.Second) / uint64(sw.videoTimeScale))
	}

	var errs []error
	if gopErr != nil {
		errs = append(errs, fmt.Errorf("flush final GOP: %w", gopErr))
	}
	if err := sw.f.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close segment: %w", err))
	}
	if flushErr != nil {
		errs = append(errs, fmt.Errorf("flush final video access unit: %w", flushErr))
	}

	return duration, errors.Join(errs...)
}

// flushGOP writes all pending video and audio samples as a single fMP4 Part.
// This produces one moof+mdat pair containing an entire GOP worth of samples.
func (sw *SegmentWriter) flushGOP() error {
	var tracks []*fmp4.PartTrack

	if len(sw.pendingVideoSamples) > 0 {
		tracks = append(tracks, &fmp4.PartTrack{
			ID:       sw.videoTrackID,
			BaseTime: sw.pendingVideoDTS,
			Samples:  sw.pendingVideoSamples,
		})
	}

	if len(sw.pendingAudioSamples) > 0 {
		tracks = append(tracks, &fmp4.PartTrack{
			ID:       sw.audioTrackID,
			BaseTime: sw.pendingAudioDTS,
			Samples:  sw.pendingAudioSamples,
		})
	}

	if len(tracks) == 0 {
		return nil
	}

	part := fmp4.Part{
		SequenceNumber: sw.seqNum,
		Tracks:         tracks,
	}

	if err := part.Marshal(sw.f); err != nil {
		return fmt.Errorf("marshal fmp4 GOP: %w", err)
	}

	if len(sw.pendingVideoSamples) > 0 {
		var written uint64
		for _, s := range sw.pendingVideoSamples {
			written += uint64(s.Duration)
		}
		sw.flushedVideoDTS = sw.pendingVideoDTS + written
	}

	sw.seqNum++
	sw.pendingVideoSamples = nil
	sw.pendingAudioSamples = nil
	sw.pendingBytes = 0

	return nil
}

func (sw *SegmentWriter) writeInit() error {
	init := fmp4.Init{
		Tracks: []*fmp4.InitTrack{
			{
				ID:        sw.videoTrackID,
				TimeScale: sw.videoTimeScale,
				Codec: &codecs.H264{
					SPS: sw.videoSPS,
					PPS: sw.videoPPS,
				},
			},
		},
	}

	if sw.hasAudio && sw.aacConfig != nil {
		init.Tracks = append(init.Tracks, &fmp4.InitTrack{
			ID:        sw.audioTrackID,
			TimeScale: sw.audioTimeScale,
			Codec: &codecs.MPEG4Audio{
				Config: *sw.aacConfig,
			},
		})
	}

	return init.Marshal(sw.f)
}
