// Package h264au normalizes H.264 access units before they are muxed or
// re-packetized.
//
// Recording, MSE, native HLS and the RTSP republisher each take the same
// depacketized access units from one camera and package them differently.
// Everything those transports need to agree on lives here: which NAL units a
// strict decoder must not see, what the in-band parameter sets currently are,
// and which level an SPS may declare. A camera quirk is then diagnosed and
// fixed once, and a viewer watching live sees the same bitstream that the
// recorder wrote to disk.
//
// Every function returns the input unchanged, sharing its backing array, when
// there is nothing to normalize, and allocates a new outer slice otherwise. The
// caller's access unit is never mutated: it is borrowed from an RTP
// depacketizer whose buffer other consumers are reading concurrently.
package h264au

import (
	"bytes"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
)

// DropSEI returns the access unit with all SEI NAL units (type 6) removed.
//
// SEI is supplemental and never required to decode. Cameras inject proprietary
// user-data SEI (TP-Link stamps a "TPLINKMARKERBOX" payload) that strict iOS
// VideoToolbox rejects as bad data (kVTVideoDecoderBadDataErr -8969), which
// collapses playback to a keyframe-only slideshow, while lenient decoders
// (browser MSE, VLC) ignore it. Dropping it is safe for every decoder and fixes
// playback for any camera that emits junk SEI.
//
// An access unit that carries nothing but SEI becomes empty; callers must treat
// an empty result as "no frame here" rather than muxing it.
func DropSEI(au [][]byte) [][]byte {
	hasSEI := false
	for _, nalu := range au {
		if isType(nalu, h264.NALUTypeSEI) {
			hasSEI = true
			break
		}
	}
	if !hasSEI {
		return au
	}
	out := make([][]byte, 0, len(au))
	for _, nalu := range au {
		if isType(nalu, h264.NALUTypeSEI) {
			continue
		}
		out = append(out, nalu)
	}
	return out
}

// TrackParameterSets folds the in-band SPS and PPS of one access unit into the
// parameter sets a transport is holding. It reports whether the SPS changed,
// which is the signal to rebuild an init segment: the codec description a
// player was handed no longer describes the stream.
//
// A camera that sends parameter sets only in the SDP produces no change here,
// so a transport seeded from the SDP keeps what it was given.
func TrackParameterSets(au [][]byte, sps, pps []byte) (newSPS, newPPS []byte, spsChanged bool) {
	newSPS, newPPS = sps, pps
	for _, nalu := range au {
		switch {
		case isType(nalu, h264.NALUTypeSPS):
			if !bytes.Equal(newSPS, nalu) {
				newSPS = nalu
				spsChanged = true
			}
		case isType(nalu, h264.NALUTypePPS):
			newPPS = nalu
		}
	}
	return newSPS, newPPS, spsChanged
}

func isType(nalu []byte, want h264.NALUType) bool {
	return len(nalu) > 0 && h264.NALUType(nalu[0]&0x1F) == want
}
