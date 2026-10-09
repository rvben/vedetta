package stream

import (
	"github.com/rvben/vedetta/internal/aac"
	"github.com/rvben/vedetta/internal/audio"
)

// hlsTranscodeSampleRate is the AAC sample rate used for G.711 cameras on
// the HLS path. G.711 is 8 kHz, but real iOS hardware AVPlayer disables an
// HLS fMP4 rendition whose AAC track is 8 kHz, so the band-limited speech
// is upsampled to this standard rate (an exact 6x integer factor from
// 8 kHz, so resampling stays artifact-free for telephony audio).
const hlsTranscodeSampleRate = 48000

// upsamplePCM resamples mono PCM by an integer factor using linear
// interpolation. G.711 is band-limited to ~3.4 kHz, so integer-factor
// linear interpolation introduces no audible artifacts while producing a
// sample rate real iOS hardware will decode. factor <= 1 is the identity.
func upsamplePCM(in []int16, factor int) []int16 {
	if factor <= 1 || len(in) == 0 {
		return in
	}
	out := make([]int16, len(in)*factor)
	for i := range in {
		cur := int32(in[i])
		next := cur
		if i+1 < len(in) {
			next = int32(in[i+1])
		}
		base := i * factor
		for f := 0; f < factor; f++ {
			// Linear step between cur and next; integer math keeps the
			// result deterministic and endpoint-exact (f=0 -> cur).
			out[base+f] = int16(cur + (next-cur)*int32(f)/int32(factor))
		}
	}
	return out
}

// aacEncoder converts 16-bit mono PCM into AAC-LC access units. The only
// production implementation is libfdk-aac loaded via purego (see
// aac_fdk.go); tests substitute a fake through newAACEncoder.
type aacEncoder interface {
	// Encode consumes PCM samples and returns zero or more complete AAC-LC
	// access units. The encoder buffers internally, so a call may return no
	// frames (input still accumulating) or several.
	Encode(pcm []int16) ([][]byte, error)
	Close()
}

// newAACEncoder builds an AAC-LC encoder for the given sample rate and
// channel count. It is a package variable so tests can inject a fake; in
// production it is the libfdk-aac-backed encoder. An error means no usable
// encoder is available, in which case the caller serves video only.
var newAACEncoder = func(sampleRate, channels int) (aacEncoder, error) {
	return aac.New(sampleRate, channels)
}

// g711IsALaw reports whether an RTSP audio codec name denotes A-law (PCMA)
// as opposed to mu-law (PCMU). Cameras advertise one or the other.
func g711IsALaw(codec string) bool { return codec == "PCMA" }

// decodeG711ToPCM turns one RTP G.711 payload into 16-bit mono PCM samples.
// The mediacommon decoders emit big-endian 16-bit LPCM bytes; we widen them
// back to int16 for the encoder.
func decodeG711ToPCM(payload []byte, aLaw bool) []int16 {
	return audio.DecodeG711(payload, aLaw)
}
