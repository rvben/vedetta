package stream

import (
	"encoding/hex"
	"fmt"

	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpmpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"

	"github.com/rvben/vedetta/internal/rtsp"
)

// videoTrackID and audioTrackID are the fMP4 track IDs used by every consumer
// in this package. Video is always track 1, audio track 2, so init segments
// and media fragments stay interchangeable across the MSE and HLS muxers.
const (
	videoTrackID = 1
	audioTrackID = 2
)

// aacSetup holds the AAC decoder plus the parameters needed to describe the
// audio track in an fMP4 init segment. Returned by newAACSetup so both the
// MSE and HLS consumers configure audio identically.
type aacSetup struct {
	decoder   *rtpmpeg4audio.Decoder
	config    *mpeg4audio.AudioSpecificConfig
	timeScale uint32
}

// newH264Decoder builds an RTP H.264 depacketizer from the SDP-advertised
// parameter sets. SPS/PPS may be empty when the camera only signals them
// in-band; the decoder still works and the consumer learns them from the
// stream.
func newH264Decoder(sps, pps []byte) (*rtsp.H264AccessUnitDecoder, error) {
	h264Format := &format.H264{
		PayloadTyp:        96,
		PacketizationMode: 1,
		SPS:               sps,
		PPS:               pps,
	}
	decoder, err := h264Format.CreateDecoder()
	if err != nil {
		return nil, err
	}
	return rtsp.NewH264AccessUnitDecoder(decoder), nil
}

// newAACSetup builds the AAC depacketizer and the matching AudioSpecificConfig
// for the given audio track. Returns nil when the track is absent or not AAC,
// so callers can treat audio as optional.
func newAACSetup(audio *rtsp.TrackInfo) (*aacSetup, error) {
	if audio == nil || audio.Codec != "AAC" {
		return nil, nil
	}

	channels := audio.ChannelCount
	if channels <= 0 {
		channels = 1
	}
	channelConfig := uint8(channels)
	if channels == 8 {
		channelConfig = 7
	}

	cfg := &mpeg4audio.AudioSpecificConfig{
		Type:          mpeg4audio.ObjectTypeAACLC,
		SampleRate:    audio.ClockRate,
		ChannelConfig: channelConfig,
	}

	aacFormat := &format.MPEG4Audio{
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
	dec, err := aacFormat.CreateDecoder()
	if err != nil {
		return nil, err
	}

	return &aacSetup{
		decoder:   dec,
		config:    cfg,
		timeScale: uint32(audio.ClockRate),
	}, nil
}

// buildFMP4Init marshals an fMP4 initialization segment (ftyp+moov) for an
// H.264 video track and an optional AAC audio track. The byte layout is
// identical to what the MSE path has shipped, so iOS native HLS and
// MediaSource both accept it unchanged.
func buildFMP4Init(sps, pps []byte, aacConfig *mpeg4audio.AudioSpecificConfig, audioTimeScale uint32) ([]byte, error) {
	if len(sps) == 0 || len(pps) == 0 {
		return nil, fmt.Errorf("no SPS/PPS available")
	}

	init := fmp4.Init{
		Tracks: []*fmp4.InitTrack{
			{
				ID:        videoTrackID,
				TimeScale: 90000,
				Codec: &codecs.H264{
					SPS: sps,
					PPS: pps,
				},
			},
		},
	}

	if aacConfig != nil {
		init.Tracks = append(init.Tracks, &fmp4.InitTrack{
			ID:        audioTrackID,
			TimeScale: audioTimeScale,
			Codec: &codecs.MPEG4Audio{
				Config: *aacConfig,
			},
		})
	}

	var buf seekableBuffer
	if err := init.Marshal(&buf); err != nil {
		return nil, fmt.Errorf("marshal init segment: %w", err)
	}
	out := make([]byte, len(buf.Bytes()))
	copy(out, buf.Bytes())
	return out, nil
}

// fmp4CodecString returns the RFC 6381 codecs string for a video/mp4 stream
// with the given H.264 SPS and optional AAC config, e.g.
// `video/mp4; codecs="avc1.640032, mp4a.40.2"`.
func fmp4CodecString(sps []byte, aacConfig *mpeg4audio.AudioSpecificConfig) string {
	videoCodec := "avc1.42001f"
	if len(sps) >= 4 {
		videoCodec = "avc1." + hex.EncodeToString(sps[1:4])
	}
	if aacConfig != nil {
		return fmt.Sprintf(`video/mp4; codecs="%s, mp4a.40.2"`, videoCodec)
	}
	return fmt.Sprintf(`video/mp4; codecs="%s"`, videoCodec)
}
